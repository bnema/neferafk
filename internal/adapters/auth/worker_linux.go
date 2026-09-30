package auth

import (
	"context"
	"crypto/subtle"
	"io"
	"os"
	"os/user"
	"strconv"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	pam "github.com/bnema/purego-pam"
	"golang.org/x/sys/unix"
)

// These seams cover only managed PAM calls; no native credentials in tests.
type pamTransaction interface {
	Authenticate(pam.Flags) error
	End() error
}
type pamBackend interface {
	start(service, user string, conversation pam.Conversation) (pamTransaction, error)
}
type managedPAM struct{ library *pam.Library }

func (p managedPAM) start(service, user string, conversation pam.Conversation) (pamTransaction, error) {
	return p.library.StartConversation(service, user, conversation)
}

func authenticate(backend pamBackend, name string, conversation pam.Conversation) ports.AuthCode {
	t, err := backend.start("neferafk", name, conversation)
	if err != nil || t == nil {
		return ports.AuthUnavailable
	}
	result := ports.AuthDenied
	// Unlocking re-verifies the logged-in user; like other screen lockers it
	// runs only the auth stack. The service file needs no account section.
	if t.Authenticate(pam.DisallowNullAuthtok) == nil {
		result = ports.AuthSuccess
	}
	if t.End() != nil {
		result = ports.AuthDenied
	}
	return result
}

// HardenProcess disables core dumps and ptrace/dump access for the calling
// process. It changes process-wide state: call it only at the entrypoint of a
// process dedicated to a secret-handling role (the isolated auth worker and the
// visual process, which holds the typed secret and PIN bootstrap), never in the
// policy daemon. PR_SET_DUMPABLE is reset by execve, and children spawned later
// inherit RLIMIT_CORE=0 only. CLI spawning remains external.
func HardenProcess() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return errSource
	}
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}); err != nil {
		return errSource
	}
	return nil
}

// MaxDenyDelay caps the pause applied before answering a wrong PIN.
const MaxDenyDelay = 30 * time.Second

// denyDelay is the pause before answering the nth consecutive wrong PIN
// (n >= 1): 1s, 2s, 4s, 8s, then MaxDenyDelay.
func denyDelay(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	if failures > 4 {
		return MaxDenyDelay
	}
	return time.Second << (failures - 1)
}

// delayClock is the time seam for the wrong-PIN delay.
type delayClock interface {
	// after returns a channel that delivers once d has elapsed.
	after(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) after(d time.Duration) <-chan time.Time { return time.After(d) }

// lookahead reads frames, optionally ahead of the main loop while a delay runs,
// so a closed input pipe ends the delay at once. Frames read early are kept in
// order for the next read.
type lookahead struct {
	r    io.Reader
	ch   chan frameRead // pending background read
	held *frameRead     // frame already read during a delay
}

type frameRead struct {
	f   ports.AuthFrame
	err error
}

func (l *lookahead) read() (ports.AuthFrame, error) {
	switch {
	case l.held != nil:
		fr := *l.held
		l.held = nil
		return fr.f, fr.err
	case l.ch != nil:
		fr := <-l.ch
		l.ch = nil
		return fr.f, fr.err
	}
	return ReadFrame(l.r)
}

// wait returns nil once d elapsed on the clock, or the read error / context
// error that interrupted it (EOF: the visual is gone).
func (l *lookahead) wait(ctx context.Context, clock delayClock, d time.Duration) error {
	if l.held == nil && l.ch == nil {
		l.ch = make(chan frameRead, 1)
		go func(ch chan<- frameRead) {
			f, err := ReadFrame(l.r)
			ch <- frameRead{f, err}
		}(l.ch)
	}
	timer := clock.after(d)
	ch := l.ch
	for {
		select {
		case <-timer:
			return nil
		case <-ctx.Done():
			if l.held != nil {
				clear(l.held.f.Payload) // never leave a read-ahead secret behind
				l.held = nil
			}
			return ctx.Err()
		case fr := <-ch:
			l.ch, ch = nil, nil
			if fr.err != nil {
				clear(fr.f.Payload)
				return fr.err
			}
			l.held = &fr
		}
	}
}

type worker struct {
	r          io.Reader
	w          io.Writer
	generation uint64
	attempt    uint64
	deadline   time.Time
	secret     []byte // dedicated secretBuffer page in production
	secretLen  int
	first      bool
}

func (w *worker) frame(kind ports.AuthKind, code ports.AuthCode) error {
	return WriteFrame(w.w, ports.AuthFrame{Kind: kind, Code: code, Generation: w.generation, Attempt: w.attempt})
}

// Respond deliberately ignores PAM text, including usernames/prompts/errors.
// All prompts use one generic masked field; multiple prompts are sequential.
func (w *worker) Respond(message pam.Message, answer []byte) (int, error) {
	if time.Now().After(w.deadline) {
		return 0, ErrProtocol
	}
	switch message.Style {
	case pam.TextInfo, pam.ErrorMsg:
		return 0, w.frame(ports.AuthStatus, ports.AuthPassword)
	case pam.PromptEchoOff, pam.PromptEchoOn:
	default:
		return 0, ErrProtocol
	}
	if w.first && message.Style == pam.PromptEchoOff {
		if w.secretLen > len(answer) {
			return 0, ErrProtocol
		}
		for _, b := range w.secret[:w.secretLen] {
			if b == 0 {
				return 0, ErrProtocol
			}
		}
		w.first = false
		n := copy(answer, w.secret[:w.secretLen])
		clear(w.secret[:])
		w.secretLen = 0
		return n, nil
	}
	if err := w.frame(ports.AuthPrompt, ports.AuthPassword); err != nil {
		return 0, err
	}
	f, err := ReadFrame(w.r)
	if err != nil {
		return 0, err
	}
	defer clear(f.Payload)
	if f.Generation != w.generation || f.Attempt != w.attempt || f.Kind != ports.AuthSecret || len(f.Payload) > len(answer) || time.Now().After(w.deadline) {
		return 0, ErrProtocol
	}
	for _, b := range f.Payload {
		if b == 0 {
			return 0, ErrProtocol
		}
	}
	return copy(answer, f.Payload), nil
}

// Run is a synchronous per-lock worker loop. Reader/writer must be private
// anonymous CLOEXEC pipes established by the visual process. The visual owner
// enforces the 30s watchdog by terminating this process: native PAM calls and
// blocked pipe reads cannot be interrupted safely by a Go goroutine.
// Bootstrap is consumed once; no PIN re-resolution occurs on wrong attempts.
func Run(ctx context.Context, r io.Reader, out io.Writer, bootstrap ports.AuthBootstrap) error {
	defer clear(bootstrap.EnvPIN)
	if bootstrap.Generation == 0 || len(bootstrap.PINReference) > ports.AuthMaxMetadata || len(bootstrap.EnvPIN) > ports.AuthMaxSecret {
		return ErrProtocol
	}
	pin := ResolvePIN(ctx, bootstrap)
	clear(bootstrap.EnvPIN)
	defer clear(pin)
	var backend pamBackend
	var library *pam.Library
	name := ""
	if pin == nil {
		if info, err := os.Stat("/etc/pam.d/neferafk"); err == nil && info.Mode().IsRegular() {
			if u, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil {
				name = u.Username
				library, _ = pam.Open()
				if library != nil {
					backend = managedPAM{library}
				}
			}
		}
	}
	if library != nil {
		defer library.Close()
	}
	return runWith(ctx, realClock{}, r, out, bootstrap.Generation, pin, backend, name, bootstrap.PINSource != "" && pin == nil)
}

func run(r io.Reader, out io.Writer, generation uint64, pin []byte, backend pamBackend, name string) error {
	return runScoped(r, out, generation, pin, backend, name, false)
}

func runScoped(r io.Reader, out io.Writer, generation uint64, pin []byte, backend pamBackend, name string, fallback bool) error {
	return runWith(context.Background(), realClock{}, r, out, generation, pin, backend, name, fallback)
}

func runWith(ctx context.Context, clock delayClock, r io.Reader, out io.Writer, generation uint64, pin []byte, backend pamBackend, name string, fallback bool) error {
	secret, err := newSecret(ports.AuthMaxSecret)
	if err != nil {
		return err
	}
	defer secret.close()
	w := &worker{r: r, w: out, generation: generation, secret: secret.data}
	if pin != nil {
		if !validPIN(pin) {
			return ErrProtocol
		}
		stored, err := newSecret(32)
		if err != nil {
			return err
		}
		defer stored.close()
		copy(stored.data, pin)
		n := len(pin)
		clear(pin) // transient Go-heap resolution copy, replaced by native page
		pin = stored.data[:n]
	}
	ready := ports.AuthPIN
	if pin == nil {
		ready = ports.AuthPassword
		if fallback {
			ready = ports.AuthFallbackPassword
		}
		if backend == nil {
			ready = ports.AuthUnavailable
		}
	}
	readyFrame := ports.AuthFrame{Kind: ports.AuthReady, Code: ready, Generation: generation}
	if fallback && backend == nil {
		readyFrame.Payload = []byte{byte(ports.AuthFallbackToPassword)}
	}
	if err := WriteFrame(out, readyFrame); err != nil {
		return err
	}
	in := &lookahead{r: r}
	failures := 0 // consecutive wrong PINs; the PAM path has pam_faildelay
	for {
		f, err := in.read()
		if err != nil {
			return err
		}
		if f.Kind != ports.AuthAttempt || f.Generation != generation || f.Attempt == 0 || f.Attempt <= w.attempt {
			clear(f.Payload)
			return ErrProtocol
		}
		for _, b := range f.Payload {
			if b == 0 {
				clear(f.Payload)
				return ErrProtocol
			}
		}
		w.attempt, w.deadline, w.first = f.Attempt, time.Now().Add(30*time.Second), true
		w.secretLen = copy(w.secret[:], f.Payload)
		clear(f.Payload)
		result := ports.AuthDenied
		if pin != nil {
			if subtle.ConstantTimeCompare(w.secret[:w.secretLen], pin) == 1 {
				result = ports.AuthSuccess
				failures = 0
			} else {
				failures++
			}
		} else if backend == nil {
			result = ports.AuthUnavailable
		} else {
			result = authenticate(backend, name, w)
		}
		if time.Now().After(w.deadline) {
			result = ports.AuthDenied
		}
		clear(w.secret[:])
		w.secretLen = 0
		if pin != nil && result == ports.AuthDenied {
			// Growing pause before every wrong-PIN answer; it ends early on
			// cancellation or when the visual closes the pipe.
			if err := in.wait(ctx, clock, denyDelay(failures)); err != nil {
				return err
			}
		}
		if err := w.frame(ports.AuthResult, result); err != nil {
			return err
		}
		if result == ports.AuthSuccess {
			return nil
		}
	}
}

package visual

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/neferclient"
	"github.com/bnema/nefergui"
)

// errLockFinished reports that the compositor refused the session lock or
// ended it (ext_session_lock_v1.finished). The lock never unlocks in response.
var errLockFinished = errors.New("session lock finished by the compositor")

// lockStatus is the authentication state shown under the input.
type lockStatus uint8

const (
	lockIdle   lockStatus = iota // ready for input
	lockBusy                     // an attempt is being verified
	lockFailed                   // the last attempt was rejected
)

// lockState is what the lock view may know: never the secret itself.
type lockState struct {
	Mask   int        // typed code points, for a mask of that many dots
	Status lockStatus // latest value received on lockConfig.Status
}

// lockConfig wires the session lock to its caller. Every callback runs on the
// lock's owner goroutine.
type lockConfig struct {
	// View builds the content of the output that hosts it: the one whose
	// surface has keyboard focus, initially the lowest output. Other outputs
	// show plain black.
	View func(*nefergui.Frame, lockState)
	// OnLocked runs once when the compositor confirms the lock.
	OnLocked func()
	// OnOutputError reports an output added after the lock was acquired that
	// could not be covered; the compositor keeps it black.
	OnOutputError func(output string, err error)
	// OnSubmit runs when Enter is pressed after locked with a non-empty
	// secret. It must not block. secret is wiped right after it returns.
	OnSubmit func(secret []byte)
	// Unlock, when it receives a value, unlocks the session once locked. It
	// is the only unlock path. Closing it is not a request.
	Unlock <-chan struct{}
	// Status delivers the state shown as lockState.Status.
	Status <-chan lockStatus
	// Wake requests a redraw.
	Wake <-chan struct{}
}

// Keys handled outside the secret buffer (xkb keysyms).
const (
	keysymReturn  = 0xff0d
	keysymKPEnter = 0xff8d
	keysymEscape  = 0xff1b
	keysymU       = 0x75
	keysymUpperU  = 0x55 // with Shift or Caps Lock
)

// lockSecretMax bounds the typed secret in code points, one more than an
// ASCII secret can use in an authentication frame. The buffer drops text past
// its capacity, so a full buffer may hold a truncated secret: Enter refuses a
// secret that fills it or whose UTF-8 bytes do not fit a frame, and never
// submits a cut one.
const lockSecretMax = ports.AuthMaxSecret + 1

// guiLocker runs ext-session-lock on its own Wayland connection with
// neferclient and draws with one NeferGUI renderer per output.
type guiLocker struct{}

// RunLock covers every output until an unlock requested on cfg.Unlock and
// returns nil. It returns errLockFinished (wrapped) when the compositor
// refuses or ends the lock, or the first failure. It never unlocks on failure
// or cancellation: closing the connection keeps the session locked.
func (guiLocker) RunLock(ctx context.Context, cfg lockConfig) (err error) {
	if cfg.View == nil {
		return errors.New("lock needs a view")
	}
	conn, err := neferclient.Connect(ctx, "")
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	l := &locker{cfg: cfg, secret: neferclient.NewSecretBuffer(lockSecretMax), initial: map[uint32]bool{}}
	l.screens = newScreens(conn)
	l.screens.onSetup = l.setupPolicy
	defer func() {
		l.secret.Wipe()
		err = errors.Join(err, conn.Close(), l.screens.closeAll())
	}()
	// Lock at once, even with no output yet: outputs that appear later are
	// covered by OutputAdded, and the session is locked meanwhile.
	if l.lock, err = conn.Lock(); err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	outputs := conn.Outputs()
	slices.SortFunc(outputs, func(a, b neferclient.Output) int { return cmp.Compare(a.Global, b.Global) })
	for _, o := range outputs {
		if err = l.cover(o); err != nil {
			return err
		}
		l.initial[o.Global] = true
	}
	l.seat = conn.Seat()
	l.seat.SetSecret(l.secret)
	defer l.seat.SetSecret(nil)
	var retry <-chan time.Time
	for {
		if err = l.wait(ctx, retry); err != nil {
			return err
		}
		if l.unlock && l.lock.Locked() {
			l.secret.Wipe()
			return l.lock.Unlock() // roundtrips: the compositor processed it
		}
		pending, err := l.screens.drawAll()
		if err != nil {
			return err
		}
		retry = nil
		if pending {
			retry = time.After(gpuRetry)
		}
	}
}

type locker struct {
	*screens

	cfg     lockConfig
	lock    *neferclient.Lock
	seat    *neferclient.Seat
	secret  *neferclient.SecretBuffer
	host    *screen
	initial map[uint32]bool // outputs present at acquisition: their failures abort
	status  lockStatus
	unlock  bool // requested; honoured once locked
}

// wait blocks for one event and applies it.
func (l *locker) wait(ctx context.Context, retry <-chan time.Time) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.conn.Wake():
		if err := l.conn.Dispatch(l); err != nil {
			return fmt.Errorf("dispatch: %w", err)
		}
	case _, ok := <-l.cfg.Unlock:
		if ok {
			l.unlock = true
		} else {
			l.cfg.Unlock = nil // closing is not a request
		}
	case st, ok := <-l.cfg.Status:
		if ok {
			l.status = st
			l.invalidateHost()
		} else {
			l.cfg.Status = nil
		}
	case <-l.cfg.Wake:
		l.invalidateHost()
	case <-retry:
	}
	return l.err
}

// cover creates the lock surface of an output.
func (l *locker) cover(o neferclient.Output) error {
	surf, err := l.lock.NewSurface(o.Global)
	if err != nil {
		return fmt.Errorf("lock surface for output %s: %w", o.Name, err)
	}
	s := newScreen(l.conn, surf, o, false)
	s.view = func(f *nefergui.Frame) { l.view(f, s) }
	l.add(s)
	if l.host == nil { // outputs are covered in ascending order: the lowest hosts
		l.setHost(s)
	}
	return nil
}

// view shows the lock content on the host output and plain black elsewhere.
func (l *locker) view(f *nefergui.Frame, s *screen) {
	if s != l.host {
		f.Root(nefergui.Inline(cssBlack))
		return
	}
	l.cfg.View(f, lockState{Mask: l.secret.Len(), Status: l.status})
}

const cssBlack = "width:100%;height:100%;background-color:#000000"

func (l *locker) setHost(s *screen) {
	if l.host == s {
		return
	}
	if l.host != nil {
		l.host.invalidate()
	}
	l.host = s
	s.invalidate()
}

func (l *locker) invalidateHost() {
	if l.host != nil {
		l.host.invalidate()
	}
}

// setupPolicy: the first renderer failure on an output present at acquisition
// aborts the lock. A failure on a hotplugged output, or a rebuild after a
// feedback change, is reported and that output dropped (the compositor keeps
// it black); the lock stays.
func (l *locker) setupPolicy(s *screen, rebuild bool, err error) error {
	err = fmt.Errorf("lock render path for output %s: %w", s.name, err)
	if l.initial[s.output] && !rebuild {
		return err
	}
	if l.cfg.OnOutputError != nil {
		l.cfg.OnOutputError(s.name, err)
	}
	return nil
}

// forgetHost picks a new host when the host screen goes away.
func (l *locker) forgetHost() {
	if l.host == nil || l.byID[l.host.surf.ID()] == l.host {
		return
	}
	l.host = nil
	for _, s := range l.byID {
		if l.host == nil || s.output < l.host.output {
			l.host = s
		}
	}
	if l.host != nil {
		l.host.invalidate()
	}
}

func (l *locker) Configure(id neferclient.SurfaceID, w, h int32) {
	l.screens.Configure(id, w, h)
	l.forgetHost()
}

func (l *locker) FeedbackDone(id neferclient.SurfaceID) {
	l.screens.FeedbackDone(id)
	l.forgetHost()
}

func (l *locker) OutputRemoved(global uint32) {
	l.screens.OutputRemoved(global)
	delete(l.initial, global)
	l.forgetHost()
}

// OutputAdded covers an output that appeared after the lock was requested.
func (l *locker) OutputAdded(o *neferclient.Output) {
	if l.lock == nil || l.err != nil || l.covers(o.Global) {
		return
	}
	if err := l.cover(*o); err != nil && l.cfg.OnOutputError != nil {
		l.cfg.OnOutputError(o.Name, err)
	}
}

// Locked confirms the lock, unless the run already failed in the same batch
// of events: it is about to return that error.
func (l *locker) Locked() {
	if l.err != nil {
		return
	}
	l.invalidateHost()
	if l.cfg.OnLocked != nil {
		l.cfg.OnLocked()
	}
}

func (l *locker) LockFinished() {
	l.fail(errors.Join(errLockFinished, l.lock.Close()))
}

func (l *locker) KeyboardFocus(id neferclient.SurfaceID, focused bool) {
	if s := l.byID[id]; s != nil && focused {
		l.setHost(s)
	}
}

func (l *locker) SecretChanged(int) { l.invalidateHost() }

// Key handles the keys that are not secret text: Enter submits, Escape and
// Ctrl+U clear. Before locked, Enter only clears.
func (l *locker) Key(ev *neferclient.KeyEvent) {
	if ev.Secret || !ev.Pressed || ev.Repeat || l.lock == nil {
		return
	}
	switch {
	case ev.Keysym == keysymReturn || ev.Keysym == keysymKPEnter:
		defer l.clearSecret() // even if OnSubmit panics
		switch {
		case !l.lock.Locked() || l.cfg.OnSubmit == nil || l.secret.Len() == 0:
		case l.secret.Len() >= lockSecretMax || len(l.secret.Bytes()) > ports.AuthMaxSecret:
			l.status = lockFailed // too long to verify: refused like a wrong secret
		default:
			l.cfg.OnSubmit(l.secret.Bytes())
		}
	case ev.Keysym == keysymEscape, (ev.Keysym == keysymU || ev.Keysym == keysymUpperU) && ev.Modifiers&neferclient.ModCtrl != 0:
		l.clearSecret()
	}
}

func (l *locker) clearSecret() {
	l.secret.Wipe()
	l.invalidateHost()
}

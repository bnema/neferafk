package visualproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/zerowrap"
)

const eventBuffer = 8
const eofGrace = 2 * time.Second
const writeTimeout = 2 * time.Second

// Launcher starts visual processes with os/exec. Child fd 3 receives commands
// and fd 4 carries events; both are private anonymous pipes (CLOEXEC on the
// daemon side). Stdout/stderr are inherited for the journal, stdin is closed.
type Launcher struct {
	Executable string
	Args       []string
	// Env is the base environment; nil means os.Environ().
	Env []string
	Log zerowrap.Logger
}

func (l *Launcher) Launch(ctx context.Context, spawn ports.VisualSpawn) (ports.VisualSession, error) {
	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	evR, evW, err := os.Pipe()
	if err != nil {
		cmdR.Close()
		cmdW.Close()
		return nil, err
	}
	env := l.Env
	if env == nil {
		env = os.Environ()
	}
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(spawn.OmitEnv, name)
	})
	cmd := exec.Command(l.Executable, l.Args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{cmdR, evW}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	cmdR.Close() // child owns its ends now
	evW.Close()
	if err != nil {
		cmdW.Close()
		evR.Close()
		return nil, fmt.Errorf("visual launch: %w", err)
	}
	s := &session{cmd: cmd, w: cmdW, r: evR, events: make(chan ports.VisualEvent, eventBuffer), detached: make(chan struct{}), exited: make(chan struct{}), log: l.Log.WithField("component", "visualproc")}
	go s.reap()
	go s.pump()
	return s, nil
}

type session struct {
	cmd      *exec.Cmd
	w, r     *os.File
	events   chan ports.VisualEvent
	detached chan struct{}
	exited   chan struct{} // closed by reap once exitErr is set
	exitErr  error
	detach   sync.Once
	log      zerowrap.Logger
}

// reap is the only caller of cmd.Wait; it outlives Detach so the child never
// becomes a zombie of the daemon.
func (s *session) reap() {
	s.exitErr = s.cmd.Wait()
	close(s.exited)
}

// pump is the only goroutine that reads the event pipe and reaps the child.
func (s *session) pump() {
	defer close(s.events)
	var perr error
	for {
		ev, err := ReadEvent(s.r)
		if err != nil {
			perr = err
			break
		}
		select {
		case s.events <- ev:
		case <-s.detached:
			return
		}
	}
	s.r.Close()
	var exit error
	select {
	case <-s.exited:
	case <-s.detached:
		return
	case <-time.After(eofGrace):
		// Closed events (or spoke garbage) yet still running: malfunction.
		s.killGroup()
		select {
		case <-s.exited:
		case <-s.detached:
			return
		}
	}
	exit = s.exitErr
	if exit == nil && perr != nil && !errors.Is(perr, io.EOF) {
		exit = perr
	}
	select {
	case s.events <- ports.VisualExited{Err: exit}:
	case <-s.detached:
	}
}

// Send never blocks the policy owner for long: a child that stops reading its
// command pipe is treated as dead by the caller.
func (s *session) Send(cmd ports.VisualCommand) error {
	_ = s.w.SetWriteDeadline(time.Now().Add(writeTimeout))
	return WriteCommand(s.w, cmd)
}
func (s *session) Events() <-chan ports.VisualEvent { return s.events }

// killGroup SIGKILLs the child's process group unless the child was already
// reaped: after Wait its pid (and so the group id) may belong to someone else.
// The remaining check-to-kill window is bounded by the reaper's Wait return.
func (s *session) killGroup() {
	select {
	case <-s.exited:
		return
	default:
	}
	if err := syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		s.log.Warn().Err(err).Msg("visual kill failed")
	}
}

// Close kills the process group; the reaper collects it and pump reports
// VisualExited. Close is idempotent.
func (s *session) Close() error {
	s.killGroup()
	err := s.w.Close()
	if err != nil && !errors.Is(err, os.ErrClosed) {
		s.log.Warn().Err(err).Msg("visual command pipe close failed")
		return err
	}
	return nil
}

// Detach abandons the child; reap collects it whenever it exits.
func (s *session) Detach() error {
	s.detach.Do(func() { close(s.detached) })
	s.r.Close()
	return s.w.Close()
}

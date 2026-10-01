package visual

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/bnema/neferafk/internal/adapters/visualproc"
	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/nefergui"
	"github.com/bnema/zerowrap"
)

var errLockWithoutAuth = errors.New("session lock ended without authentication success")

// lockRunner is the session-lock seam over nefergui.RunLock.
type lockRunner interface {
	RunLock(ctx context.Context, cfg nefergui.LockConfig) error
}

type guiLocker struct{}

func (guiLocker) RunLock(ctx context.Context, cfg nefergui.LockConfig) error {
	return nefergui.RunLock(ctx, cfg)
}

// Config wires the visual process: fd 3 commands in, fd 4 events out.
type Config struct {
	Commands   io.Reader
	Events     io.Writer
	Executable string // neferafk binary that hosts `auth-worker`
	Log        zerowrap.Logger
}

// Run serves the daemon until the lock was released, a reveal finished, or the
// command pipe closed with nothing held. It returns nil for those exits.
func Run(ctx context.Context, cfg Config) error {
	s := &session{
		log: cfg.Log.WithField("component", "visual"), cmds: cfg.Commands, events: cfg.Events,
		locker: guiLocker{}, fader: guiFader{}, launcher: execLauncher{exe: cfg.Executable},
	}
	return s.run(ctx)
}

// ExecutablePath is the running binary, for spawning the auth worker.
func ExecutablePath() (string, error) { return os.Executable() }

type session struct {
	log      zerowrap.Logger
	cmds     io.Reader
	events   io.Writer
	locker   lockRunner
	fader    fadeRunner
	launcher workerLauncher
}

type cmdMsg struct {
	cmd ports.VisualCommand
	err error
}

type fadeRun struct {
	m      *fadeModel
	cancel context.CancelFunc
	done   chan error
}

type lockRun struct {
	gen      ports.Generation
	boot     ports.AuthBootstrap
	ctl      *authController
	cancel   context.CancelFunc
	done     chan error    // RunLock result
	authDone chan struct{} // auth controller stopped and worker killed
	locked   chan struct{}
}

func (s *session) readCommands(ctx context.Context, out chan<- cmdMsg) {
	for {
		c, err := visualproc.ReadCommand(s.cmds)
		select {
		case out <- cmdMsg{cmd: c, err: err}:
		case <-ctx.Done():
			wipeCommand(c)
			return
		}
		if err != nil {
			return
		}
	}
}

func wipeCommand(c ports.VisualCommand) {
	if l, ok := c.(ports.VisualLock); ok {
		clear(l.Auth.EnvPIN)
	}
}

// run is the owner loop: it alone holds the fade and lock state.
func (s *session) run(ctx context.Context) error {
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	cmdCh := make(chan cmdMsg)
	go s.readCommands(rctx, cmdCh)
	cmdC := (<-chan cmdMsg)(cmdCh)
	var fd *fadeRun
	var lk *lockRun
	stopFade := func() {
		if fd != nil {
			fd.cancel()
			<-fd.done
			fd = nil
		}
	}
	defer func() { stopFade() }()
	for {
		var lockDone <-chan error
		var lockedC <-chan struct{}
		var fadeDone <-chan error
		sig := ctx.Done()
		if lk != nil {
			// A held lock ignores termination requests: only unlock ends it.
			lockDone, lockedC, sig = lk.done, lk.locked, nil
		}
		if fd != nil {
			fadeDone = fd.done
		}
		select {
		case <-sig:
			return nil
		case m := <-cmdC:
			if m.err != nil {
				cmdC = nil
				s.log.Info().Msg("daemon command pipe closed")
				if lk == nil {
					return nil // fade-only: nothing left to protect
				}
				continue // daemon gone: the lock and its auth UI stay up
			}
			switch c := m.cmd.(type) {
			case ports.VisualFade:
				switch {
				case fd != nil:
					if !c.Black || lk == nil {
						fd.m.send(fadeCmd{black: c.Black, dur: c.Duration})
					}
				case lk != nil:
					// lock surfaces already cover (or will cover) the screen
				case !c.Black:
					return nil // nothing to reveal
				default:
					fd = s.startFade(ctx)
					fd.m.send(fadeCmd{black: true, dur: c.Duration})
				}
			case ports.VisualLock:
				if lk != nil {
					if lk.gen != c.Generation {
						s.log.Warn().Msg("lock for another generation ignored")
					}
					clear(c.Auth.EnvPIN)
					continue
				}
				lk = s.startLock(ctx, c)
			}
		case <-lockedC:
			lk.locked = nil
			if err := visualproc.WriteEvent(s.events, ports.LockConfirmed{Generation: lk.gen}); err != nil {
				s.log.Warn().Err(err).Msg("lock confirmation not delivered")
			}
			stopFade()
		case err := <-fadeDone:
			fd = nil
			if err != nil && !errors.Is(err, context.Canceled) {
				s.log.Error().Err(err).Msg("fade failed")
				if lk == nil {
					return err
				}
				continue
			}
			if lk == nil {
				return nil // reveal finished
			}
		case err := <-lockDone:
			lk.cancel()
			<-lk.authDone
			// The UI may have queued a submission after the controller left.
			lk.ctl.drainSubmits()
			clear(lk.boot.EnvPIN)
			gen, succeeded := lk.gen, lk.ctl.succeeded.Load()
			lk = nil
			if err != nil {
				s.log.Error().Err(err).Msg("session lock ended without unlock")
				return err
			}
			if !succeeded {
				// RunLock returning nil is not an authorization: only the
				// worker's AuthSuccess may release the lock.
				s.log.Error().Msg("session lock ended without authentication success")
				return errLockWithoutAuth
			}
			if werr := visualproc.WriteEvent(s.events, ports.LockReleased{Generation: gen}); werr != nil {
				s.log.Warn().Err(werr).Msg("lock release not delivered")
			}
			return nil
		}
	}
}

func (s *session) startFade(ctx context.Context) *fadeRun {
	fctx, cancel := context.WithCancel(ctx)
	f := &fadeRun{m: newFadeModel(), cancel: cancel, done: make(chan error, 1)}
	go func() { f.done <- s.fader.RunFade(fctx, f.m) }()
	return f
}

// startLock runs RunLock with the V1 view and a private auth controller. The
// lock context is detached from process signals: it ends only with RunLock.
func (s *session) startLock(ctx context.Context, c ports.VisualLock) *lockRun {
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ctl := newAuthController(s.log, s.launcher, c.Auth, workerTimeout)
	l := &lockRun{
		gen: c.Generation, boot: c.Auth, ctl: ctl, cancel: cancel,
		done: make(chan error, 1), authDone: make(chan struct{}), locked: make(chan struct{}, 1),
	}
	go func() { defer close(l.authDone); ctl.run(lctx) }()
	go clockWake(lctx.Done(), ctl.wake)
	go func() {
		secret := nefergui.NewSecretBuffer(ports.AuthMaxSecret)
		err := s.locker.RunLock(lctx, lockConfig(s.log, ctl, secret, l.locked))
		secret.Wipe()
		l.done <- err
	}()
	return l
}

// lockConfig wires NeferGUI's lock loop to the auth controller. OnSubmit only
// copies the secret; the controller wipes the copy after writing it.
func lockConfig(log zerowrap.Logger, ctl *authController, secret *nefergui.SecretBuffer, locked chan<- struct{}) nefergui.LockConfig {
	log = log.WithField("component", "visual-lock")
	return nefergui.LockConfig{
		Secret: secret,
		View: func(f *nefergui.Frame, st nefergui.LockState) {
			lockView(f, st, promptKind(ctl.prompt.Load()), time.Now())
		},
		OnLocked: func() {
			select {
			case locked <- struct{}{}:
			default:
			}
		},
		OnOutputError: func(output string, err error) {
			// A hotplugged output we could not cover; the compositor keeps it
			// black. Only the output name and the error are logged.
			log.Warn().Err(err).Str("output", output).Msg("lock surface not created for output")
		},
		OnSubmit: ctl.submit,
		Unlock:   ctl.unlock,
		Status:   ctl.status,
		Wake:     ctl.wake,
	}
}

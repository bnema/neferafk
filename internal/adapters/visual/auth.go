package visual

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/bnema/neferafk/internal/adapters/auth"
	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/nefergui"
	"github.com/bnema/zerowrap"
)

// workerTimeout is the watchdog for worker startup and for one attempt. PAM
// calls cannot be interrupted from Go, so expiry kills the worker process. It
// includes the worker's longest wrong-PIN delay: killing a worker mid-delay
// would respawn it with the delay schedule reset.
const workerTimeout = 30*time.Second + auth.MaxDenyDelay

// workerLauncher starts one isolated `neferafk auth-worker` process.
type workerLauncher interface {
	launch(ctx context.Context) (workerProcess, error)
}

// workerProcess is one running worker with its private pipes: stdin carries the
// bootstrap record and frames to the worker, stdout carries frames back.
type workerProcess interface {
	pipes() (stdin io.WriteCloser, stdout io.ReadCloser)
	// kill terminates the process (not a foreign call in this process).
	kill() error
	// wait reaps the process; it blocks until the process is gone.
	wait() error
}

// promptKind is what the lock box asks for. It is metadata, never typed input.
type promptKind uint32

const (
	promptNone        promptKind = iota // worker not ready yet
	promptPIN                           // PIN source resolved
	promptPassword                      // plain account password
	promptFallback                      // PIN source unavailable: password
	promptUnavailable                   // no usable authentication
	promptMore                          // PAM asked for another response
)

type phase uint8

const (
	phaseNoWorker phase = iota // none alive; the next attempt respawns
	phaseStarting              // bootstrap sent, waiting for Ready
	phaseReady                 // waiting for a submission
	phaseAttempt               // attempt sent, waiting for the result
	phasePrompt                // worker asked for another secret
	phaseDone                  // success; nothing further is accepted
)

type frameMsg struct {
	f   ports.AuthFrame
	err error
}

type liveWorker struct {
	proc   workerProcess
	in     io.WriteCloser
	out    io.ReadCloser
	frames chan frameMsg
	stop   chan struct{}
}

// authController owns the auth state for one lock. Only its run goroutine
// touches the fields below "owned"; it never blocks the UI loop, which talks to
// it through channels. Typed secrets arrive as private copies on submits and
// are wiped as soon as they have been written to the worker pipe.
type authController struct {
	log      zerowrap.Logger
	launcher workerLauncher
	boot     ports.AuthBootstrap // EnvPIN is owned by the caller
	timeout  time.Duration

	submits chan []byte
	status  chan nefergui.LockStatus
	unlock  chan struct{}
	wake    chan struct{}

	prompt    atomic.Uint32
	succeeded atomic.Bool

	// owned by run:
	w       *liveWorker
	ph      phase
	attempt uint64
	pending []byte
	dog     *time.Timer
	dogC    <-chan time.Time
}

func newAuthController(log zerowrap.Logger, l workerLauncher, boot ports.AuthBootstrap, timeout time.Duration) *authController {
	return &authController{
		log: log.WithField("component", "visual-auth"), launcher: l, boot: boot, timeout: timeout,
		submits: make(chan []byte, 1), status: make(chan nefergui.LockStatus, 8),
		unlock: make(chan struct{}, 1), wake: make(chan struct{}, 1),
	}
}

// submit is called on the UI loop. It copies the secret (the caller wipes its
// own) and never blocks; a submission racing a running attempt is dropped.
func (c *authController) submit(secret []byte) {
	cp := append([]byte(nil), secret...)
	select {
	case c.submits <- cp:
	default:
		clear(cp)
	}
}

func (c *authController) setStatus(s nefergui.LockStatus) {
	for {
		select {
		case c.status <- s:
			return
		default:
			select {
			case <-c.status:
			default:
			}
		}
	}
}

func (c *authController) setPrompt(p promptKind) {
	if c.prompt.Swap(uint32(p)) != uint32(p) {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

func (c *authController) arm() {
	if c.dog == nil {
		c.dog = time.NewTimer(c.timeout)
	} else {
		if !c.dog.Stop() {
			select {
			case <-c.dog.C:
			default:
			}
		}
		c.dog.Reset(c.timeout)
	}
	c.dogC = c.dog.C
}

func (c *authController) disarm() {
	if c.dog != nil && !c.dog.Stop() {
		select {
		case <-c.dog.C:
		default:
		}
	}
	c.dogC = nil
}

// run serves the lock until ctx ends or authentication succeeded.
func (c *authController) run(ctx context.Context) {
	defer c.drop()
	c.spawn(ctx)
	for {
		var frames <-chan frameMsg
		if c.w != nil {
			frames = c.w.frames
		}
		select {
		case <-ctx.Done():
			return
		case s := <-c.submits:
			c.onSubmit(ctx, s)
		case m := <-frames:
			if c.onFrame(m) {
				return
			}
		case <-c.dogC:
			c.log.Warn().Msg("authentication worker timed out; killing it")
			c.dogC = nil
			c.workerLost(true)
		}
	}
}

func (c *authController) drop() {
	c.disarm()
	c.killWorker()
	clear(c.pending)
	c.pending = nil
	c.drainSubmits()
}

// drainSubmits wipes any submission left in the queue (the UI may have handed
// one over after the last receive). Safe to call again once run has returned.
func (c *authController) drainSubmits() {
	for {
		select {
		case s := <-c.submits:
			clear(s)
		default:
			return
		}
	}
}

func (c *authController) spawn(ctx context.Context) {
	proc, err := c.launcher.launch(ctx)
	if err != nil {
		c.log.Warn().Err(err).Msg("authentication worker launch failed")
		c.ph = phaseNoWorker
		return
	}
	in, out := proc.pipes()
	w := &liveWorker{proc: proc, in: in, out: out, frames: make(chan frameMsg), stop: make(chan struct{})}
	if err := auth.WriteBootstrap(in, c.boot); err != nil {
		c.log.Warn().Msg("authentication worker bootstrap failed")
		c.w = w
		c.killWorker()
		return
	}
	c.w, c.ph = w, phaseStarting
	go readFrames(w)
	c.arm()
}

func readFrames(w *liveWorker) {
	for {
		f, err := auth.ReadFrame(w.out)
		select {
		case w.frames <- frameMsg{f: f, err: err}:
		case <-w.stop:
			clear(f.Payload)
			return
		}
		if err != nil {
			return
		}
	}
}

// killWorker terminates and reaps the current worker, if any.
func (c *authController) killWorker() {
	w := c.w
	if w == nil {
		return
	}
	c.w = nil
	close(w.stop)
	_ = w.proc.kill()
	_ = w.in.Close()
	_ = w.out.Close()
	go func() { _ = w.proc.wait() }()
}

// workerLost handles a dead, timed-out or misbehaving worker: the lock stays,
// the next attempt respawns.
func (c *authController) workerLost(failed bool) {
	inFlight := c.ph == phaseAttempt || c.ph == phasePrompt || c.pending != nil
	c.disarm()
	c.killWorker()
	clear(c.pending)
	c.pending = nil
	c.ph = phaseNoWorker
	if c.prompt.Load() == uint32(promptMore) {
		c.setPrompt(promptNone)
	}
	if failed || inFlight {
		c.setStatus(nefergui.LockFailed)
	}
}

func (c *authController) onSubmit(ctx context.Context, s []byte) {
	switch c.ph {
	case phaseReady:
		c.sendAttempt(s)
	case phasePrompt:
		c.sendSecret(s)
	case phaseNoWorker:
		clear(c.pending)
		c.pending = s
		c.setStatus(nefergui.LockBusy)
		c.spawn(ctx)
		if c.w == nil { // launch failed
			clear(c.pending)
			c.pending = nil
			c.setStatus(nefergui.LockFailed)
		}
	case phaseStarting: // keep only the latest submission
		clear(c.pending)
		c.pending = s
	default: // attempt running or done: nothing to accept
		clear(s)
	}
}

func (c *authController) write(f ports.AuthFrame) bool {
	err := auth.WriteFrame(c.w.in, f)
	clear(f.Payload)
	return err == nil
}

func (c *authController) sendAttempt(s []byte) {
	c.attempt++
	c.setStatus(nefergui.LockBusy)
	c.ph = phaseAttempt
	c.arm()
	if !c.write(ports.AuthFrame{Kind: ports.AuthAttempt, Generation: c.boot.Generation, Attempt: c.attempt, Payload: s}) {
		c.workerLost(true)
	}
}

func (c *authController) sendSecret(s []byte) {
	c.setStatus(nefergui.LockBusy)
	c.ph = phaseAttempt
	c.arm()
	if !c.write(ports.AuthFrame{Kind: ports.AuthSecret, Generation: c.boot.Generation, Attempt: c.attempt, Payload: s}) {
		c.workerLost(true)
	}
}

// onFrame applies one worker message; it reports true once authentication
// succeeded and the unlock was requested.
func (c *authController) onFrame(m frameMsg) bool {
	if m.err != nil {
		c.log.Warn().Msg("authentication worker ended")
		c.workerLost(false)
		return false
	}
	f := m.f
	clear(f.Payload)
	if f.Generation != c.boot.Generation || (f.Kind != ports.AuthReady && f.Attempt != c.attempt) {
		c.log.Warn().Msg("authentication worker sent a frame for another epoch")
		c.workerLost(true)
		return false
	}
	switch f.Kind {
	case ports.AuthReady:
		if c.ph != phaseStarting {
			c.workerLost(true)
			return false
		}
		c.disarm()
		c.ph = phaseReady
		switch f.Code {
		case ports.AuthPIN:
			c.setPrompt(promptPIN)
		case ports.AuthFallbackPassword:
			c.setPrompt(promptFallback)
		case ports.AuthUnavailable:
			c.setPrompt(promptUnavailable)
		default:
			c.setPrompt(promptPassword)
		}
		if p := c.pending; p != nil {
			c.pending = nil
			c.sendAttempt(p)
		}
	case ports.AuthPrompt:
		if c.ph != phaseAttempt {
			c.workerLost(true)
			return false
		}
		c.ph = phasePrompt
		c.setPrompt(promptMore)
		c.setStatus(nefergui.LockIdle)
	case ports.AuthStatus:
		// PAM text is deliberately never shown.
	case ports.AuthResult:
		if c.ph != phaseAttempt && c.ph != phasePrompt {
			c.workerLost(true)
			return false
		}
		c.disarm()
		if f.Code == ports.AuthSuccess {
			c.ph = phaseDone
			c.succeeded.Store(true)
			c.unlock <- struct{}{} // cap 1, sent once
			return true
		}
		c.ph = phaseReady
		c.setStatus(nefergui.LockFailed)
	default:
		c.workerLost(true)
	}
	return false
}

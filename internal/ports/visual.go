package ports

import (
	"context"
	"time"
)

// Visual is the policy daemon's only handle on the separate visual process
// (fade animation and ext-session-lock surfaces). The daemon never initialises
// Vulkan or fonts; an implementation spawns the visual lazily on first use.
//
// There is deliberately NO unlock method: only a successful authentication in
// the visual+auth-worker pair releases the lock, and the visual reports that
// as a LockReleased event. Activity, reload, logind Unlock and control-socket
// requests can never reach an unlock path through this port.
//
// Methods are called from the single policy owner goroutine only. Results and
// child lifecycle come back asynchronously as VisualEvent values on the events
// channel the implementation was constructed with (channel port; tested by
// sending messages).
type Visual interface {
	// Fade starts (Black) or reverses (!Black) the fade. A reveal with no
	// running visual is a no-op and never spawns a process. Idempotent. A
	// reveal never drops lock surfaces: it only undoes the fade animation.
	Fade(ctx context.Context, req VisualFade) error
	// Lock makes the visual acquire ext-session-lock for req.Generation,
	// spawning it if needed. Idempotent for a live visual already asked for
	// the same generation, and safe to call again after VisualExited to
	// re-spawn: a crashed lock visual leaves the compositor locked and the
	// respawn re-acquires. req.Auth.EnvPIN ownership transfers to the
	// implementation, which wipes it once written to the private pipe.
	Lock(ctx context.Context, req VisualLock) error
	// Close kills and reaps any visual process. Killing a lock visual leaves
	// the compositor locked; it never unlocks.
	Close() error
	// Detach closes only the daemon's pipe ends and leaves the child running.
	// The daemon uses it instead of Close when exiting while a lock is live:
	// the visual treats command-pipe EOF as "daemon gone", keeps its lock and
	// authentication UI, and exits only after a successful unlock.
	Detach() error
}

// VisualCommand is daemon -> visual traffic on the private pipe.
type VisualCommand interface{ visualCommand() }

// VisualFade: Duration is ports.Config.FadeDuration.
type VisualFade struct {
	Black    bool
	Duration time.Duration
	Spawn    VisualSpawn
}

// VisualSpawn applies only when the request has to launch a process. OmitEnv
// names environment variables the child must not inherit (the configured PIN
// variable: its value travels solely in VisualLock.Auth.EnvPIN).
type VisualSpawn struct{ OmitEnv []string }

// VisualLock carries auth metadata only (config + the privately captured
// startup PIN value when the env source is selected). Auth.Generation equals
// Generation and is the nonzero epoch scoping every auth frame. The visual,
// not the daemon, spawns the auth worker and forwards Auth to it. Output is
// Config.LockOutput.
type VisualLock struct {
	Generation Generation
	Auth       AuthBootstrap
	Output     string
	Spawn      VisualSpawn
}

func (VisualFade) visualCommand() {}
func (VisualLock) visualCommand() {}

// VisualEvent is visual -> daemon traffic (lifecycle results, never secrets).
// LockConfirmed and LockReleased are the authoritative lock lifecycle results
// and double as PolicyEvents; VisualExited is the final event of a process.
type VisualEvent interface{ visualEvent() }

// VisualExited reports that the visual process is gone (crash, kill or normal
// exit after unlock/reveal). Err is nil for a clean zero exit. It is never an
// unlock: a lock without a prior LockReleased is still held by the compositor.
type VisualExited struct{ Err error }

func (LockConfirmed) visualEvent() {}
func (LockReleased) visualEvent()  {}
func (VisualExited) visualEvent()  {}

// VisualLauncher is the os/exec adapter seam: it starts one visual process
// with a private anonymous pipe pair (child fd 3 = commands in, fd 4 = events
// out) and returns its session, honouring spawn.OmitEnv.
type VisualLauncher interface {
	Launch(ctx context.Context, spawn VisualSpawn) (VisualSession, error)
}

// VisualSession is one running visual process. Send is called from the owner
// goroutine while Events is read from another goroutine.
type VisualSession interface {
	// Send writes a command; it fails once the process is gone.
	Send(cmd VisualCommand) error
	// Events yields child events; the final one is VisualExited, then the
	// channel closes.
	Events() <-chan VisualEvent
	// Close kills (SIGKILL) and reaps the process. Idempotent.
	Close() error
	// Detach closes the daemon-side pipe ends and stops observing the child.
	Detach() error
}

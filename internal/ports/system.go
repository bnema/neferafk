package ports

import "time"

// System events are observations, never authentication/unlock authority.
type SystemEvent interface{ systemEvent() }
type SystemCapabilities struct {
	Session              bool
	Suspend              bool
	SuspendAuthorization string // yes, challenge, no, na
	DelayMax             time.Duration
	Missing              []string
}
type SystemCapabilityReport struct{ Capabilities SystemCapabilities }
type SystemLockRequested struct{}

// SystemUnlockObserved must NEVER become LockReleased for an acquired locker.
type SystemUnlockObserved struct{}

// MaxDelay is logind's configured maximum, NOT remaining time or a promise of
// an arbitrary grace period. ReceivedAt is local receipt, not sleep start.
type SleepPreparation struct {
	Cycle      uint64
	Preparing  bool
	ReceivedAt time.Time
	MaxDelay   time.Duration
}
type SystemFailure struct {
	Operation string
	Err       error
}

func (SystemCapabilityReport) systemEvent() {}
func (SystemLockRequested) systemEvent()    {}
func (SystemUnlockObserved) systemEvent()   {}
func (SleepPreparation) systemEvent()       {}
func (SystemFailure) systemEvent()          {}

type SystemCommand interface{ systemCommand() }

// SystemSleepReady releases only the inhibitor for this preparation cycle.
type SystemSleepReady struct{ Cycle uint64 }
type SystemSuspend struct{}

func (SystemSleepReady) systemCommand() {}
func (SystemSuspend) systemCommand()    {}

type SystemRequirements struct{ Session, Sleep bool }

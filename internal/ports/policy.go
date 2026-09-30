package ports

import "time"

type Generation uint64
type Action uint8

const (
	Lock Action = iota
	Fade
	OutputOff
	Sleep
)

// Input contains no credentials. LockReleased is an authoritative visual-owner
// lifecycle result, never an activity/reload request to unlock.
type PolicyEvent interface{ policyEvent() }
type IdleDue struct {
	Generation Generation
	// Notification identifies an existing proxy across unrelated reloads.
	Notification uint64
	After        time.Duration
}
type Activity struct{}
type Reload struct{ Config Config }
type LockConfirmed struct{ Generation Generation }
type LockReleased struct{ Generation Generation }
type BeforeSleep struct{}

func (IdleDue) policyEvent()       {}
func (Activity) policyEvent()      {}
func (Reload) policyEvent()        {}
func (LockConfirmed) policyEvent() {}
func (LockReleased) policyEvent()  {}
func (BeforeSleep) policyEvent()   {}

// One notification per distinct timeout and registration origin; After is
// relative to registration. Equal timeout values after a reload may have
// different IDs: retaining an old origin must not rebase unchanged deadlines.
// Actions sharing one notification dispatch in Lock/Fade/OutputOff/Sleep order.
// Replacement destroys old proxies. Generation fences their queued events.
type IdleNotification struct {
	// ID is stable while this pending deadline is unchanged by reload.
	// Adapters retain that proxy (and its old admitting Generation), avoiding
	// recreation that would invent an exact current idle age.
	ID    uint64
	After time.Duration
}
type PolicyEffect interface{ policyEffect() }
type ArmIdle struct {
	Generation    Generation
	Notifications []IdleNotification
}
type AcquireLock struct{ Generation Generation }
type SetFade struct {
	Black    bool
	Duration time.Duration
}
type SetOutputPower struct{ On bool }
type Suspend struct{}

// SleepReady lets the system adapter release its delay inhibitor. No sleep is
// induced by this response; it acknowledges a BeforeSleep already in progress.
type SleepReady struct{}

func (ArmIdle) policyEffect()        {}
func (AcquireLock) policyEffect()    {}
func (SetFade) policyEffect()        {}
func (SetOutputPower) policyEffect() {}
func (Suspend) policyEffect()        {}
func (SleepReady) policyEffect()     {}

// LockRequested asks policy to acquire the session lock now (logind Lock
// signal or the private control socket). It is idempotent while a lock is
// being acquired or held and never orders/forces any other action.
type LockRequested struct{}

func (LockRequested) policyEvent() {}

package ports

import "time"

// Wayland commands contain no credentials or lock ownership. The visual owns
// ext-session-lock; this connection observes capabilities, idle and power only.
type WaylandCommand interface{ waylandCommand() }
type SetDeadlines struct {
	Generation uint64
	Deadlines  []time.Duration
}
type OutputPower struct{ On bool }

func (SetDeadlines) waylandCommand() {}
func (OutputPower) waylandCommand()  {}

type WaylandEvent interface{ waylandEvent() }

// Proxy and Cycle identify one notification lifetime and its idle/resume cycle.
// After is a distinct deadline, not an inferred policy action. Core maps it.
type WaylandIdle struct {
	Generation, Proxy, Cycle uint64
	Seat                     uint32
	After                    time.Duration
}
type WaylandActivity struct {
	Proxy, Cycle uint64
	Seat         uint32
}
type WaylandCapabilities struct{ Capabilities Capabilities }
type WaylandPowerFailed struct{ Output uint64 }

func (WaylandIdle) waylandEvent()         {}
func (WaylandActivity) waylandEvent()     {}
func (WaylandCapabilities) waylandEvent() {}
func (WaylandPowerFailed) waylandEvent()  {}

type WaylandOutput struct {
	Lifetime       uint64
	Global         uint32
	Name           string
	PowerSupported bool
}
type Capabilities struct {
	Globals map[string]uint32
	Seats   []uint32
	Outputs []WaylandOutput
	Missing []string
}

// Requirements describe enabled features, not a particular compositor brand.
type WaylandRequirements struct {
	Idle, InputWake, OutputPower, Lock, Visual bool
}

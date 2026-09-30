package ports

// Private local control (unix socket). Only Lock and Status exist: there is no
// unlock request and none may be added; unlock is authentication-only.
type ControlKind uint8

const (
	ControlLock ControlKind = iota + 1
	ControlStatus
)

// ControlRequest is delivered to the policy owner, which answers exactly once
// on Reply (buffered, capacity >= 1).
type ControlRequest struct {
	Kind  ControlKind
	Reply chan<- ControlReply
}

// ControlReply is a state snapshot after the request was applied. Lock returns
// as soon as acquisition was requested, not when it was confirmed.
type ControlReply struct {
	Generation                       Generation
	Acquiring, Protected, Faded, Off bool
	Err                              string
}

package ports

// Auth messages are private visual↔worker traffic, never policy-daemon events.
const (
	AuthMaxSecret   = 512
	AuthMaxMetadata = 4096
)

type AuthKind uint8

const (
	AuthReady AuthKind = iota + 1
	AuthAttempt
	AuthPrompt
	AuthSecret
	AuthStatus
	AuthResult
	AuthCancel
)

type AuthCode uint8

const (
	AuthPassword AuthCode = iota + 1
	AuthPIN
	AuthUnavailable
	AuthDenied
	AuthSuccess
	AuthInvalid
	// AuthFallbackPassword means a selected PIN source was unavailable or
	// invalid and the visual must ask for an account password instead.
	AuthFallbackPassword
)

type AuthFallback uint8

const AuthFallbackToPassword AuthFallback = 1

// Payload ownership transfers to the recipient, which must clear secrets.
// Every frame is scoped to a nonzero lock epoch. Ready alone uses attempt 0.
// Ready has no payload except AuthUnavailable may carry exactly one byte
// AuthFallbackToPassword when fallback was required but PAM is unavailable.
// Attempt/Secret have code 0 and bounded secret bytes. All other kinds have
// no payload; Prompt/Status use AuthPassword, Result uses success/denied/
// unavailable/invalid, Cancel uses AuthInvalid. Non-Ready attempts are nonzero.
type AuthFrame struct {
	Kind       AuthKind
	Code       AuthCode
	Generation uint64
	Attempt    uint64
	Payload    []byte
}

// AuthBootstrap is delivered on a private anonymous pipe once per lock.
// EnvPIN is the privately captured startup environment value, never a lookup
// of the authentication worker's environment. Run consumes and wipes it.
type AuthBootstrap struct {
	Generation   uint64
	PINSource    PINSource
	PINReference string
	EnvPIN       []byte
}

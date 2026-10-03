package ports

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxDuration = 24 * time.Hour

// Zero After disables only that action. No deadline implies another action.
type Config struct {
	LockAfter, FadeAfter, OffAfter, SleepAfter time.Duration
	FadeDuration                               time.Duration
	Auth                                       AuthConfig
	// LockOutput names the only output (wl_output name, e.g. DP-2) that
	// shows the lock prompt while it is connected. Empty, or absent: every
	// output shows it. A change applies from the next lock.
	LockOutput string
}

// MaxOutputName bounds LockOutput: neferclient truncates longer wl_output
// names, which could never match.
const MaxOutputName = 64

type AuthMode string

const (
	AuthModePassword AuthMode = "password"
	AuthModePIN      AuthMode = "pin"
)

type PINSource string

const (
	PINSourceEnv  PINSource = "env"
	PINSourcePass PINSource = "pass"
)

// Only metadata: values resolved by the visual/auth worker never reach policy.
// PIN mode always falls back to PAM when its selected source is unavailable or
// invalid. A wrong PIN is authentication failure, never a fallback trigger.
type AuthConfig struct {
	Mode             AuthMode
	PINSource        PINSource
	PINEnv, PINEntry string
}

func Defaults() Config {
	return Config{LockAfter: 5 * time.Minute, FadeAfter: 6 * time.Minute, FadeDuration: 2 * time.Second, OffAfter: 7 * time.Minute, SleepAfter: 20 * time.Minute, Auth: AuthConfig{Mode: AuthModePassword}}
}
func (c Config) Validate() error {
	for _, d := range []time.Duration{c.LockAfter, c.FadeAfter, c.OffAfter, c.SleepAfter, c.FadeDuration} {
		if d < 0 || d > MaxDuration {
			return fmt.Errorf("duration outside 0..24h")
		}
	}
	if len(c.LockOutput) > MaxOutputName || !utf8.ValidString(c.LockOutput) || strings.ContainsFunc(c.LockOutput, unicode.IsControl) {
		return fmt.Errorf("invalid lock output name")
	}
	a := c.Auth
	for _, r := range []string{string(a.Mode), string(a.PINSource), a.PINEnv, a.PINEntry} {
		if !utf8.ValidString(r) {
			return fmt.Errorf("auth metadata is not valid UTF-8")
		}
	}
	if a.Mode != AuthModePassword && a.Mode != AuthModePIN {
		return fmt.Errorf("unknown auth mode")
	}
	for _, r := range []string{a.PINEnv, a.PINEntry} {
		if len(r) > 4096 {
			return fmt.Errorf("auth metadata too large")
		}
		for _, c := range r {
			if unicode.IsControl(c) {
				return fmt.Errorf("control character in auth metadata")
			}
		}
	}
	if a.PINEnv != "" {
		for i, r := range a.PINEnv {
			if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
				return fmt.Errorf("invalid environment reference")
			}
		}
	}
	if a.PINEntry != "" {
		if strings.HasPrefix(a.PINEntry, "-") || strings.HasPrefix(a.PINEntry, "/") || strings.Contains(a.PINEntry, "\\") {
			return fmt.Errorf("invalid pass entry")
		}
		for _, part := range strings.Split(a.PINEntry, "/") {
			if part == "" || part == "." || part == ".." {
				return fmt.Errorf("invalid pass entry")
			}
		}
	}
	switch a.PINSource {
	case "":
		if a.Mode == AuthModePIN || a.PINEnv != "" || a.PINEntry != "" {
			return fmt.Errorf("PIN metadata requires source")
		}
	case PINSourceEnv:
		if a.PINEnv == "" || a.PINEntry != "" {
			return fmt.Errorf("env source requires only pin-env")
		}
	case PINSourcePass:
		if a.PINEntry == "" || a.PINEnv != "" {
			return fmt.Errorf("pass source requires only pin-entry")
		}
	default:
		return fmt.Errorf("unknown PIN source")
	}
	return nil
}

type ConfigChanged struct{ Config Config }

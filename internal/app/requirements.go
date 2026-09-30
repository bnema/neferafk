package app

import (
	"fmt"
	"strings"

	"github.com/bnema/neferafk/internal/ports"
)

// Requirements derives what the daemon needs from its startup config. Idle
// notification/input-wake and lock/visual protocols are always required: a
// daemon that cannot observe activity or lock is unsupported, and manual lock
// (logind Lock, control socket) needs the lock protocols even with lock.after
// off. Output power and sleep support are required only when enabled; enabling
// them later by live reload needs a restart (a warning is logged).
func Requirements(cfg ports.Config) (ports.WaylandRequirements, ports.SystemRequirements) {
	return ports.WaylandRequirements{Idle: true, InputWake: true, OutputPower: cfg.OffAfter > 0, Lock: true, Visual: true},
		ports.SystemRequirements{Session: true, Sleep: cfg.SleepAfter > 0 || cfg.LockAfter > 0}
}

// UnsupportedError lists every missing capability, one per line.
type UnsupportedError struct{ Missing []string }

func (e *UnsupportedError) Error() string {
	return "unsupported environment:\n  - " + strings.Join(e.Missing, "\n  - ")
}

const suspendMissing = "non-interactive suspend"

// unsupported merges adapter reports. Missing non-interactive suspend never
// blocks startup: fade, lock and output-off still run and only the sleep
// action is skipped (suspendWarning). The sleep delay inhibitor stays required
// because before-sleep locking depends on it.
func unsupported(wl *ports.Capabilities, sys *ports.SystemCapabilities) []string {
	var missing []string
	if wl != nil {
		for _, m := range wl.Missing {
			missing = append(missing, "wayland: "+m)
		}
	}
	if sys != nil {
		if !sys.Session {
			missing = append(missing, "logind: current session")
		}
		for _, m := range sys.Missing {
			if strings.HasPrefix(m, suspendMissing) {
				continue // degrades only the sleep action, see suspendWarning
			}
			missing = append(missing, "logind: "+m)
		}
	}
	return missing
}

// suspendWarning reports why an enabled sleep action cannot suspend, or "".
func suspendWarning(cfg ports.Config, sys *ports.SystemCapabilities) string {
	if cfg.SleepAfter == 0 || sys == nil || sys.Suspend {
		return ""
	}
	return fmt.Sprintf("suspend unavailable (%s); sleep.after is skipped", sys.SuspendAuthorization)
}

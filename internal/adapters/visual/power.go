package visual

import (
	"time"

	"bnema.dev/neferafk/internal/ports"
	"bnema.dev/neferclient"
)

// Power shortcut keys (xkb keysyms).
const (
	keysymF1       = 0xffbe
	keysymF2       = 0xffbf
	keysymF3       = 0xffc0
	keysymControlL = 0xffe3
	keysymControlR = 0xffe4
)

const (
	// powerHoldTime is how long Ctrl+F2/F3 must be held to fire.
	powerHoldTime = time.Second
	// powerTick redraws the hold progress while a hold runs.
	powerTick = 40 * time.Millisecond
)

// powerHold tracks the lock screen power shortcuts: Ctrl+F1 suspends at
// once, Ctrl+F2 (reboot) and Ctrl+F3 (poweroff) fire after a full hold. It
// belongs to the lock owner goroutine and never sees secret text. Ctrl must
// be physically held for the whole hold: a latched (sticky) Ctrl is cancelled
// by ModifiersChanged once the latch releases, which is intended.
type powerHold struct {
	fire   func(ports.PowerAction)
	key    uint32 // held keysym; 0 when no hold runs
	action ports.PowerAction
	start  time.Time
	ticker *time.Ticker
}

// keyEvent applies one key event and reports whether it changed the hold, so
// the caller redraws. locked is false before the compositor confirmed the
// lock: no shortcut works then.
func (h *powerHold) keyEvent(ev *neferclient.KeyEvent, locked bool) (changed bool) {
	if h.key != 0 {
		ctrlGone := ev.Modifiers&neferclient.ModCtrl == 0 || ev.Keysym == keysymControlL || ev.Keysym == keysymControlR
		switch {
		case ev.Repeat && ev.Keysym == h.key:
			return false
		case ev.Pressed || ev.Keysym == h.key || ctrlGone:
			h.cancel() // a new press may still start a shortcut below
			changed = true
		default:
			return false
		}
	}
	if !locked || h.fire == nil || ev.Secret || !ev.Pressed || ev.Repeat ||
		ev.Modifiers&(neferclient.ModCtrl|neferclient.ModAlt|neferclient.ModSuper|neferclient.ModShift) != neferclient.ModCtrl {
		return changed
	}
	switch ev.Keysym {
	case keysymF1:
		h.fire(ports.PowerSuspend)
	case keysymF2:
		h.begin(ev.Keysym, ports.PowerReboot)
	case keysymF3:
		h.begin(ev.Keysym, ports.PowerOff)
	default:
		return changed
	}
	return true
}

func (h *powerHold) begin(key uint32, a ports.PowerAction) {
	h.cancel() // never leak a running ticker
	h.key, h.action, h.start = key, a, time.Now()
	h.ticker = time.NewTicker(powerTick)
}

// cancel ends a hold without firing and reports whether one was running.
func (h *powerHold) cancel() bool {
	running := h.key != 0
	if h.ticker != nil {
		h.ticker.Stop()
	}
	h.key, h.action, h.ticker = 0, 0, nil
	return running
}

// tick is the redraw channel, nil when no hold runs.
func (h *powerHold) tick() <-chan time.Time {
	if h.ticker == nil {
		return nil
	}
	return h.ticker.C
}

// progress is the hold fraction in (0,1] at now, 0 without a hold.
func (h *powerHold) progress(now time.Time) float64 {
	if h.key == 0 {
		return 0
	}
	return max(min(float64(now.Sub(h.start))/float64(powerHoldTime), 1), 1e-3)
}

// advance fires the action once the hold is complete and ends the hold.
func (h *powerHold) advance(now time.Time) {
	if h.key != 0 && now.Sub(h.start) >= powerHoldTime {
		a := h.action
		h.cancel()
		h.fire(a)
	}
}

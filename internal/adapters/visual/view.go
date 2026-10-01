package visual

import (
	"strings"
	"time"

	"github.com/bnema/nefergui"
)

// Lock look: black screen, one monospace box with a grey border. The masked
// input sits at the exact screen center: the date line above the box and an
// equal-height spacer below it balance each other, and an empty spacer inside
// the box balances the status line under the input. The view sees only the
// typed-character count, never the secret.
const (
	cssScreen = "display:flex;flex-direction:column;align-items:center;justify-content:center;" +
		"width:100%;height:100%;background-color:#000000;color:#ffffff;font-family:monospace;font-size:18px"
	// cssFrame fixes the shared width so the date line ends at the box edge.
	cssFrame = "display:flex;flex-direction:column;align-items:stretch;width:360px"
	cssClock = "color:#ffffff;text-align:right;height:24px;margin-bottom:8px"
	// cssBalance mirrors cssClock below the box.
	cssBalance = "height:24px;margin-top:8px"
	cssBox     = "display:flex;flex-direction:column;align-items:stretch;border:2px solid #bfbfbf;" +
		"padding:24px;background-color:#000000"
	// The box content is 360-2*2-2*24 = 308px wide; each field is centered by
	// an explicit left margin: (308 - (width + 16 padding + 2 border)) / 2.
	// The input field: an inverted relief, bottom and right edges at 50% white.
	cssMask = "color:#ffffff;height:32px;line-height:32px;width:280px;padding:4px 8px;margin-left:5px;" +
		"border-bottom:2px solid rgba(255,255,255,0.5);border-right:2px solid rgba(255,255,255,0.5)"
	// cssMaskPIN: a short field with large, centered bullets.
	cssMaskPIN = "color:#ffffff;height:32px;line-height:32px;width:160px;padding:4px 8px;margin-left:65px;" +
		"font-size:24px;letter-spacing:6px;text-align:center;" +
		"border-bottom:2px solid rgba(255,255,255,0.5);border-right:2px solid rgba(255,255,255,0.5)"
	// cssHint and cssHintBalance have the same outer height.
	cssHint        = "color:#ffffff;margin-top:12px;height:22px;text-align:center"
	cssHintBalance = "margin-bottom:12px;height:22px"

	clockLayout = "Mon 02 Jan 2006  15:04"
)

// hintFor is the line under the input: the attempt status first, then a note
// for prompts the bare field cannot convey. It never carries typed input.
func hintFor(s nefergui.LockStatus, p promptKind) string {
	switch s {
	case nefergui.LockBusy:
		return "..."
	case nefergui.LockFailed:
		return "Denied"
	}
	switch p {
	case promptFallback:
		return "PIN unavailable, use password"
	case promptUnavailable:
		return "Authentication unavailable"
	case promptMore:
		return "Enter again"
	}
	return " "
}

// maskFor is the field content and style: large dots for a PIN, bullets
// plus a cursor for a password.
func maskFor(s nefergui.LockState, p promptKind) (string, string) {
	if p == promptPIN {
		return strings.Repeat("●", s.Mask), cssMaskPIN
	}
	return s.Bullets() + "_", cssMask
}

// UnlockBox draws the date line, the box and the balancing spacer.
func UnlockBox(root nefergui.Node, s nefergui.LockState, p promptKind, now time.Time) {
	frame := root.Column(nefergui.Class("frame"), nefergui.Inline(cssFrame))
	frame.Text(now.Format(clockLayout), nefergui.Key("clock"), nefergui.Inline(cssClock))
	box := frame.Column(nefergui.Class("box"), nefergui.Key("box"), nefergui.Inline(cssBox))
	box.Spacer(nefergui.Key("hint-balance"), nefergui.Inline(cssHintBalance))
	mask, css := maskFor(s, p)
	box.Text(mask, nefergui.Key("mask"), nefergui.Inline(css))
	box.Text(hintFor(s.Status, p), nefergui.Key("hint"), nefergui.Inline(cssHint))
	frame.Spacer(nefergui.Key("balance"), nefergui.Inline(cssBalance))
}

// lockView builds the whole lock screen.
func lockView(f *nefergui.Frame, s nefergui.LockState, p promptKind, now time.Time) {
	UnlockBox(f.Root(nefergui.Class("screen"), nefergui.Inline(cssScreen)), s, p, now)
}

// clockWake requests a redraw at every minute boundary until ctx ends, so
// the date line stays current. A pending request is enough: sends never block.
func clockWake(done <-chan struct{}, wake chan<- struct{}) {
	t := time.NewTimer(untilNextMinute(time.Now()))
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			select {
			case wake <- struct{}{}:
			default:
			}
			t.Reset(untilNextMinute(now))
		}
	}
}

func untilNextMinute(now time.Time) time.Duration {
	return now.Truncate(time.Minute).Add(time.Minute).Sub(now)
}

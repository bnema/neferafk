package visual

import (
	"fmt"
	"strings"
	"time"

	"github.com/bnema/nefergui"
)

// Lock look: black screen, one monospace box with a grey border. The masked
// input sits at the exact screen center: the date line above the box and an
// equal-height spacer below it balance each other, and an empty spacer inside
// the box balances the status line under the input. The view sees only the
// typed-character count, never the secret.
//
// NeferGUI's flex layout does not center a fixed-width child across the
// column here, so each field gets an explicit left margin derived from the
// geometry below.
const (
	frameW     = 360 // shared width: the date line ends at the box edge
	boxBorder  = 2
	boxPadding = 24
	fieldPadX  = 8
	fieldEdge  = 2 // relief width (bottom and right)
	passwordW  = 280
	pinW       = 160

	// boxContentW is the width available to the field inside the box.
	boxContentW = frameW - 2*boxBorder - 2*boxPadding

	// Monospace advance at 18px is about 10.8px: 28 characters fit the
	// box content, 24 bullets plus the cursor fit the password field, and
	// 7 dots at 24px plus spacing fit the PIN field.
	maxHintRunes    = 28
	maxPasswordMask = 24
	maxPINMask      = 7

	cssScreen = "display:flex;flex-direction:column;align-items:center;justify-content:center;" +
		"width:100%;height:100%;background-color:#000000;color:#ffffff;font-family:monospace;font-size:18px"
	cssClock = "color:#ffffff;text-align:right;height:24px;margin-bottom:8px"
	// cssBalance mirrors cssClock below the box.
	cssBalance = "height:24px;margin-top:8px"
	// cssHint and cssHintBalance have the same outer height.
	cssHint        = "color:#ffffff;margin-top:12px;height:22px;text-align:center"
	cssHintBalance = "margin-bottom:12px;height:22px"

	clockLayout = "Mon 02 Jan 2006  15:04"
)

var (
	// cssRelief is the inverted relief: bottom and right edges at 50% white.
	cssRelief = fmt.Sprintf("border-bottom:%[1]dpx solid rgba(255,255,255,0.5);"+
		"border-right:%[1]dpx solid rgba(255,255,255,0.5)", fieldEdge)
	cssFrame = fmt.Sprintf("display:flex;flex-direction:column;align-items:stretch;width:%dpx", frameW)
	cssBox   = fmt.Sprintf("display:flex;flex-direction:column;align-items:stretch;"+
		"border:%dpx solid #bfbfbf;padding:%dpx;background-color:#000000", boxBorder, boxPadding)
	cssMask = fmt.Sprintf("color:#ffffff;height:32px;line-height:32px;width:%dpx;"+
		"padding:4px %dpx;margin-left:%dpx;%s", passwordW, fieldPadX, fieldMargin(passwordW), cssRelief)
	cssMaskPIN = fmt.Sprintf("color:#ffffff;height:32px;line-height:32px;width:%dpx;"+
		"padding:4px %dpx;margin-left:%dpx;font-size:24px;letter-spacing:6px;text-align:center;%s",
		pinW, fieldPadX, fieldMargin(pinW), cssRelief)
)

// fieldMargin centers a field of content width w inside the box content.
func fieldMargin(w int) int {
	return (boxContentW - (w + 2*fieldPadX + fieldEdge)) / 2
}

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
		return "PIN off, use password"
	case promptUnavailable:
		return "Authentication unavailable"
	case promptMore:
		return "Enter again"
	}
	return " "
}

// maskFor is the field content and style: large dots for a PIN, bullets
// plus a cursor for a password. The count is capped to the field width.
func maskFor(s nefergui.LockState, p promptKind) (string, string) {
	if p == promptPIN {
		return strings.Repeat("●", min(s.Mask, maxPINMask)), cssMaskPIN
	}
	return strings.Repeat("•", min(s.Mask, maxPasswordMask)) + "_", cssMask
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

// clockPoll is short because the monotonic timer stops during suspend: a
// lock held across suspend must show the current minute soon after resume.
const clockPoll = 5 * time.Second

// clockWake requests a redraw whenever the wall-clock minute changes, until
// done closes. Sends never block: a pending request is enough.
func clockWake(done <-chan struct{}, wake chan<- struct{}) {
	t := time.NewTicker(clockPoll)
	defer t.Stop()
	shown := time.Now().Truncate(time.Minute)
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if m := time.Now().Truncate(time.Minute); !m.Equal(shown) {
				shown = m
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}
	}
}

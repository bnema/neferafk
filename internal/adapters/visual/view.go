package visual

import (
	"fmt"
	"math"
	"strings"
	"time"

	"bnema.dev/nefergui"
)

// Lock look: black screen, one monospace box with a grey border. The masked
// input sits at the exact screen center: the date line above the box (the
// lock time while Ctrl is held) and an equal-height spacer below it balance
// each other, and an
// empty spacer inside the box balances the indicator bar and status line
// under the input. The indicator bar is a 1px line under the field, always
// laid out: a hold progress fill, or full white while Caps Lock is on. The
// view sees only the typed-character count, never the secret.
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
	// Row heights around the field. The balances are written out: tests
	// check they equal the rows they mirror.
	clockH       = 24 // date line
	boxGap       = 8  // between the date line and the box
	balanceH     = 24 // below the box: clockH
	barGap       = 4  // between the field and the bar
	barH         = 1
	hintGap      = 12 // between the bar and the hint
	hintH        = 22
	hintBalanceH = 27 // above the field: barGap + barH + hintH

	clockLayout  = "Mon 02 Jan 2006  15:04"
	lockedLayout = "locked 15:04"
)

var (
	cssClock = clockCSS("#ffffff")
	// cssLocked is cssClock dimmed, for the lock time.
	cssLocked = clockCSS("rgba(255,255,255,0.5)")
	// cssBalance mirrors the date line below the box.
	cssBalance = fmt.Sprintf("height:%dpx;margin-top:%dpx", balanceH, boxGap)
	// cssHintBalance mirrors the bar and cssHint above the field.
	cssHint = hintCSS("#ffffff")
	// cssPowerHelp is cssHint dimmed, for the power shortcut help.
	cssPowerHelp   = hintCSS("rgba(255,255,255,0.5)")
	cssHintBalance = fmt.Sprintf("margin-bottom:%dpx;height:%dpx", hintGap, hintBalanceH)
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
	return (boxContentW - fieldOuter(w)) / 2
}

// fieldOuter is the outer width of a field of content width w.
func fieldOuter(w int) int { return w + 2*fieldPadX + fieldEdge }

// barWidth is the white part of the indicator bar under a field of content
// width w: the hold progress first, else the whole field while Caps Lock is
// on, else nothing.
func barWidth(s lockState, w int) int {
	outer := fieldOuter(w)
	switch {
	case s.Hold > 0:
		return int(math.Round(min(s.Hold, 1) * float64(outer)))
	case s.Caps:
		return outer
	}
	return 0
}

// barCSS styles the indicator bar. Its box always takes the same space; only
// the white width changes. align-self keeps the box stretch from overriding
// the width, and min-height lifts the 8px minimum NeferGUI gives spacers.
func barCSS(s lockState, p promptKind) string {
	w := passwordW
	if p == promptPIN {
		w = pinW
	}
	white := barWidth(s, w)
	bg := "transparent"
	if white > 0 {
		bg = "#ffffff"
	}
	return fmt.Sprintf("align-self:flex-start;height:%dpx;min-height:0px;margin-top:%dpx;margin-left:%dpx;width:%dpx;background-color:%s",
		barH, barGap, fieldMargin(w), white, bg)
}

// hintCSS styles the line under the input in color; every hint keeps the
// same height so swapping them never shifts the layout.
func hintCSS(color string) string {
	return fmt.Sprintf("color:%s;margin-top:%dpx;height:%dpx;text-align:center", color, hintGap, hintH)
}

// clockCSS styles the date line in color; the date and the lock time share
// it so swapping them never shifts the layout.
func clockCSS(color string) string {
	return fmt.Sprintf("color:%s;text-align:right;height:%dpx;margin-bottom:%dpx", color, clockH, boxGap)
}

// clockLine is the text and style of the line above the box: the lock time
// while Ctrl reveals it, else the date.
func clockLine(s lockState, now time.Time) (string, string) {
	if s.Ctrl && !s.LockedAt.IsZero() {
		return s.LockedAt.Format(lockedLayout), cssLocked
	}
	return now.Format(clockLayout), cssClock
}

const powerHelpText = "F1 sleep  F2 reboot  F3 off"

// hintLine is the text and style of the line under the input: the power
// shortcut help while Ctrl is held, else hintFor.
func hintLine(s lockState, p promptKind) (string, string) {
	if s.PowerHelp {
		return powerHelpText, cssPowerHelp
	}
	return hintFor(s, p), cssHint
}

// hintFor is the line under the input: the attempt status first, then a note
// for prompts the bare field cannot convey. It never carries typed input.
func hintFor(s lockState, p promptKind) string {
	switch s.Status {
	case lockBusy:
		return "..."
	case lockFailed:
		if s.Failures > 1 {
			return fmt.Sprintf("Denied ×%d", s.Failures)
		}
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
func maskFor(s lockState, p promptKind) (string, string) {
	if p == promptPIN {
		return strings.Repeat("●", min(s.Mask, maxPINMask)), cssMaskPIN
	}
	return strings.Repeat("•", min(s.Mask, maxPasswordMask)) + "_", cssMask
}

// UnlockBox draws the date line, the box and the balancing spacer.
func UnlockBox(root nefergui.Node, s lockState, p promptKind, now time.Time) {
	frame := root.Column(nefergui.Class("frame"), nefergui.Inline(cssFrame))
	clock, css := clockLine(s, now)
	frame.Text(clock, nefergui.Key("clock"), nefergui.Inline(css))
	box := frame.Column(nefergui.Class("box"), nefergui.Key("box"), nefergui.Inline(cssBox))
	box.Spacer(nefergui.Key("hint-balance"), nefergui.Inline(cssHintBalance))
	mask, css := maskFor(s, p)
	box.Text(mask, nefergui.Key("mask"), nefergui.Inline(css))
	box.Spacer(nefergui.Key("bar"), nefergui.Inline(barCSS(s, p)))
	hint, hcss := hintLine(s, p)
	box.Text(hint, nefergui.Key("hint"), nefergui.Inline(hcss))
	frame.Spacer(nefergui.Key("balance"), nefergui.Inline(cssBalance))
}

// lockView builds the whole lock screen.
func lockView(f *nefergui.Frame, s lockState, p promptKind, now time.Time) {
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

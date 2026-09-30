package visual

import (
	"github.com/bnema/nefergui"
)

// V1 lock look: black screen, one centered monospace box with a white border,
// masked input (bullets only). No clock, avatar or widgets. The view sees only
// the typed-character count, never the secret.
const (
	cssScreen = "display:flex;flex-direction:column;align-items:center;justify-content:center;" +
		"width:100%;height:100%;background-color:#000000;color:#ffffff;font-family:monospace;font-size:18px"
	cssBox = "display:flex;flex-direction:column;align-items:center;border:2px solid #ffffff;" +
		"padding:24px;min-width:320px;background-color:#000000"
	cssLabel = "color:#ffffff"
	// The input field: a visible white-underlined line, bullets plus a cursor.
	cssMask = "color:#ffffff;min-height:28px;min-width:280px;margin-top:12px;padding:4px 8px;" +
		"border-bottom:2px solid #ffffff"
	cssHint = "color:#ffffff;margin-top:12px;min-height:22px"
)

// labelFor is the fixed, non-secret caption for the requested credential.
func labelFor(p promptKind) string {
	switch p {
	case promptPIN:
		return "PIN"
	case promptFallback:
		return "Password (PIN unavailable)"
	case promptUnavailable:
		return "Authentication unavailable"
	case promptMore:
		return "Password (again)"
	case promptPassword:
		return "Password"
	}
	return "Locked"
}

// hintFor is the minimal failure/busy line.
func hintFor(s nefergui.LockStatus) string {
	switch s {
	case nefergui.LockBusy:
		return "..."
	case nefergui.LockFailed:
		return "Denied"
	}
	return " "
}

// UnlockBox draws the centered box.
func UnlockBox(root nefergui.Node, s nefergui.LockState, p promptKind) {
	box := root.Column(nefergui.Class("box"), nefergui.Inline(cssBox))
	box.Text(labelFor(p), nefergui.Key("label"), nefergui.Inline(cssLabel))
	box.Text(s.Bullets()+"_", nefergui.Key("mask"), nefergui.Inline(cssMask))
	box.Text(hintFor(s.Status), nefergui.Key("hint"), nefergui.Inline(cssHint))
}

// lockView builds the whole lock screen.
func lockView(f *nefergui.Frame, s nefergui.LockState, p promptKind) {
	UnlockBox(f.Root(nefergui.Class("screen"), nefergui.Inline(cssScreen)), s, p)
}

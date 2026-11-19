package visual

import (
	"fmt"
	"testing"
	"time"
	"unicode/utf8"

	"bnema.dev/neferclient"
	"github.com/stretchr/testify/require"
)

func TestFadeAlphaInterpolatesAndArrives(t *testing.T) {
	t0 := time.Unix(100, 0)
	a, done := fadeAlpha(0, 1, t0, 2*time.Second, t0.Add(time.Second))
	require.InDelta(t, 0.5, a, 1e-9)
	require.False(t, done)
	a, done = fadeAlpha(0, 1, t0, 2*time.Second, t0.Add(3*time.Second))
	require.Equal(t, 1.0, a)
	require.True(t, done)
	_, done = fadeAlpha(1, 0, t0, 0, t0)
	require.True(t, done)
}

// Surfaces share the model but draw at different instants: a surface that
// drew just after the end must not stop redraws for one that drew just before.
func TestFadeKeepsTickingUntilEverySurfaceDrewTheEnd(t *testing.T) {
	m := newFadeModel()
	m.send(fadeCmd{black: true, dur: time.Second})
	t0 := m.base.Add(time.Minute)
	a, done := m.step(t0)
	require.Equal(t, 0.0, a)
	require.False(t, done)

	before := t0.Add(time.Second - time.Millisecond)
	after := t0.Add(time.Second + time.Millisecond)
	_, done = m.step(before) // surface A
	require.False(t, done)
	a, done = m.step(after) // surface B reaches the end
	require.Equal(t, 1.0, a)
	require.True(t, done)
	require.True(t, m.animatingAt(after), "surface A still needs a redraw")
	require.False(t, m.animatingAt(t0.Add(time.Hour)))
}

func TestFadeLayerIsClickThroughOverlayOnEveryOutput(t *testing.T) {
	l := fadeLayer("DP-1")
	require.Equal(t, "DP-1", l.Output)
	require.Equal(t, neferclient.LayerOverlay, l.Level)
	require.Equal(t, neferclient.KeyboardNone, l.Keyboard)
	require.Equal(t, int32(-1), l.ExclusiveZone)
	require.NotNil(t, l.InputRects)
	require.Empty(t, l.InputRects)
	all := neferclient.AnchorTop | neferclient.AnchorBottom | neferclient.AnchorLeft | neferclient.AnchorRight
	require.Equal(t, all, l.Anchors)
}

func TestHintsShowStatusThenPromptNotesAndFitTheBox(t *testing.T) {
	for _, tc := range []struct {
		s    lockStatus
		p    promptKind
		want string
	}{
		{lockFailed, promptFallback, "Denied"},
		{lockFailed, promptMore, "Denied"},
		{lockBusy, promptPIN, "..."},
		{lockIdle, promptFallback, "PIN off, use password"},
		{lockIdle, promptUnavailable, "Authentication unavailable"},
		{lockIdle, promptMore, "Enter again"},
		{lockIdle, promptPassword, " "},
		{lockIdle, promptPIN, " "},
	} {
		got := hintFor(lockState{Status: tc.s, Failures: 1}, tc.p)
		require.Equal(t, tc.want, got)
		require.LessOrEqual(t, utf8.RuneCountInString(got), maxHintRunes, got)
	}
}

func TestDeniedCountsRepeatedFailures(t *testing.T) {
	require.Equal(t, "Denied", hintFor(lockState{Status: lockFailed}, promptPassword))
	require.Equal(t, "Denied", hintFor(lockState{Status: lockFailed, Failures: 1}, promptPassword))
	got := hintFor(lockState{Status: lockFailed, Failures: 3}, promptPIN)
	require.Equal(t, "Denied ×3", got)
	require.LessOrEqual(t, utf8.RuneCountInString(got), maxHintRunes)
	require.Equal(t, " ", hintFor(lockState{Status: lockIdle, Failures: 3}, promptPassword))
}

func TestBarShowsHoldThenCaps(t *testing.T) {
	for _, w := range []int{passwordW, pinW} {
		outer := fieldOuter(w)
		require.Equal(t, 0, barWidth(lockState{}, w))
		require.Equal(t, outer, barWidth(lockState{Caps: true}, w))
		require.Equal(t, outer/2, barWidth(lockState{Caps: true, Hold: 0.5}, w), "hold wins over caps")
		require.Equal(t, outer, barWidth(lockState{Hold: 1.5}, w))
	}
	require.Contains(t, barCSS(lockState{}, promptPassword), "background-color:transparent")
	css := barCSS(lockState{Caps: true}, promptPIN)
	require.Contains(t, css, "height:1px;min-height:0") // the UA sheet gives spacers min-height:8px
	require.Contains(t, css, fmt.Sprintf("width:%dpx", fieldOuter(pinW)))
	require.Contains(t, css, fmt.Sprintf("margin-left:%dpx", fieldMargin(pinW)))
	require.Contains(t, css, "background-color:#ffffff")
}

func TestLockTimeShowsOnlyOnceLocked(t *testing.T) {
	require.Equal(t, " ", lockedText(time.Time{}))
	require.Equal(t, "locked 09:07", lockedText(time.Date(2026, 1, 2, 9, 7, 0, 0, time.Local)))
}

// The spacers mirror the rows around the field so it stays centered.
func TestBalancesMirrorAddedRows(t *testing.T) {
	require.Equal(t, clockH+lockedH, balanceH)
	require.Equal(t, barGap+barH+hintH, hintBalanceH)
	require.Contains(t, barCSS(lockState{}, promptPassword), fmt.Sprintf("height:%dpx;min-height:0px;margin-top:%dpx", barH, barGap))
}

func TestMaskIsCappedToTheField(t *testing.T) {
	mask, _ := maskFor(lockState{Mask: 3}, promptPIN)
	require.Equal(t, "●●●", mask)
	mask, _ = maskFor(lockState{Mask: 3}, promptFallback)
	require.Equal(t, "•••_", mask)
	mask, _ = maskFor(lockState{Mask: 512}, promptPIN)
	require.Equal(t, maxPINMask, utf8.RuneCountInString(mask))
	mask, _ = maskFor(lockState{Mask: 512}, promptPassword)
	require.Equal(t, maxPasswordMask+1, utf8.RuneCountInString(mask))
}

func TestFieldsAreCenteredInTheBox(t *testing.T) {
	for _, w := range []int{passwordW, pinW} {
		outer := w + 2*fieldPadX + fieldEdge
		require.Equal(t, boxContentW-outer-fieldMargin(w), fieldMargin(w), w)
	}
}

func TestClockWakeStopsWhenDone(t *testing.T) {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() { defer close(stopped); clockWake(done, make(chan struct{})) }()
	close(done)
	<-stopped
}

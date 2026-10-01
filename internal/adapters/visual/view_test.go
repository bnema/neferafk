package visual

import (
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bnema/nefergui"
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
	l := fadeLayer()
	require.True(t, l.AllOutputs)
	require.Empty(t, l.Output)
	require.Equal(t, nefergui.LayerOverlay, l.Level)
	require.Equal(t, nefergui.KeyboardNone, l.Keyboard)
	require.NotNil(t, l.InputRects)
	require.Empty(t, l.InputRects)
	all := nefergui.AnchorTop | nefergui.AnchorBottom | nefergui.AnchorLeft | nefergui.AnchorRight
	require.Equal(t, all, l.Anchors)
}

func TestHintsShowStatusThenPromptNotesAndFitTheBox(t *testing.T) {
	for _, tc := range []struct {
		s    nefergui.LockStatus
		p    promptKind
		want string
	}{
		{nefergui.LockFailed, promptFallback, "Denied"},
		{nefergui.LockFailed, promptMore, "Denied"},
		{nefergui.LockBusy, promptPIN, "..."},
		{nefergui.LockIdle, promptFallback, "PIN off, use password"},
		{nefergui.LockIdle, promptUnavailable, "Authentication unavailable"},
		{nefergui.LockIdle, promptMore, "Enter again"},
		{nefergui.LockIdle, promptPassword, " "},
		{nefergui.LockIdle, promptPIN, " "},
	} {
		got := hintFor(tc.s, tc.p)
		require.Equal(t, tc.want, got)
		require.LessOrEqual(t, utf8.RuneCountInString(got), maxHintRunes, got)
	}
}

func TestMaskIsCappedToTheField(t *testing.T) {
	mask, _ := maskFor(nefergui.LockState{Mask: 3}, promptPIN)
	require.Equal(t, "●●●", mask)
	mask, _ = maskFor(nefergui.LockState{Mask: 3}, promptFallback)
	require.Equal(t, "•••_", mask)
	mask, _ = maskFor(nefergui.LockState{Mask: 512}, promptPIN)
	require.Equal(t, maxPINMask, utf8.RuneCountInString(mask))
	mask, _ = maskFor(nefergui.LockState{Mask: 512}, promptPassword)
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

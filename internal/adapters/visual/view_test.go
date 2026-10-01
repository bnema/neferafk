package visual

import (
	"testing"
	"time"

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

func TestLabelsAndHintsNeverCarryInput(t *testing.T) {
	require.Equal(t, "Locked", labelFor(promptNone))
	require.Equal(t, "PIN", labelFor(promptPIN))
	require.Equal(t, "Denied", hintFor(nefergui.LockFailed))
	require.Equal(t, "...", hintFor(nefergui.LockBusy))
}

package visual

import (
	"testing"

	"github.com/bnema/neferclient"
	"github.com/stretchr/testify/require"
)

// boundLocker builds a locker over screens keyed by surface ID. Only output
// selection runs here: neither drawing nor the connection is used.
func boundLocker(output string, byID map[neferclient.SurfaceID]*screen) *locker {
	l := &locker{cfg: lockConfig{Output: output}, screens: &screens{byID: byID}}
	l.rebind()
	return l
}

func TestLockContentShowsOnEveryOutputByDefault(t *testing.T) {
	hdmi := &screen{output: 49, name: "HDMI-A-1"}
	dp := &screen{output: 50, name: "DP-2"}

	l := boundLocker("", map[neferclient.SurfaceID]*screen{1: hdmi, 2: dp})
	require.True(t, l.shows(hdmi))
	require.True(t, l.shows(dp))

	l = boundLocker("DP-9", map[neferclient.SurfaceID]*screen{1: hdmi, 2: dp})
	require.True(t, l.shows(hdmi), "an absent lock.output shows the content everywhere")
	require.True(t, l.shows(dp))
}

func TestLockOutputBindsContentWhileCovered(t *testing.T) {
	hdmi := &screen{output: 49, name: "HDMI-A-1"}
	dp := &screen{output: 50, name: "DP-2"}
	byID := map[neferclient.SurfaceID]*screen{1: hdmi, 2: dp}

	l := boundLocker("DP-2", byID)
	require.False(t, l.shows(hdmi), "other outputs show black")
	require.True(t, l.shows(dp))

	delete(byID, 2) // DP-2 unplugged
	l.rebind()
	require.True(t, l.shows(hdmi), "the content returns to every output")

	byID[3] = dp // DP-2 back
	l.rebind()
	require.False(t, l.shows(hdmi))
	require.True(t, l.shows(dp))
}

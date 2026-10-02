package visual

import (
	"testing"

	"github.com/bnema/neferclient"
	"github.com/stretchr/testify/require"
)

// hostLocker builds a locker over screens keyed by surface ID. Only host
// selection runs here: neither drawing nor the connection is used.
func hostLocker(output string, byID map[neferclient.SurfaceID]*screen) *locker {
	return &locker{cfg: lockConfig{Output: output}, screens: &screens{byID: byID}}
}

func TestKeyboardFocusMovesHostOnlyWithoutPreferredOutput(t *testing.T) {
	hdmi := &screen{output: 49, name: "HDMI-A-1"}
	dp := &screen{output: 50, name: "DP-2"}

	l := hostLocker("", map[neferclient.SurfaceID]*screen{1: hdmi, 2: dp})
	l.setHost(hdmi)
	l.KeyboardFocus(2, true)
	require.Same(t, dp, l.host, "without lock.output the prompt follows keyboard focus")

	l = hostLocker("DP-2", map[neferclient.SurfaceID]*screen{1: hdmi, 2: dp})
	l.setHost(dp)
	l.KeyboardFocus(1, true)
	require.Same(t, dp, l.host, "the preferred output keeps the prompt")

	l = hostLocker("DP-9", map[neferclient.SurfaceID]*screen{1: hdmi, 2: dp})
	l.setHost(hdmi)
	l.KeyboardFocus(2, true)
	require.Same(t, dp, l.host, "an absent preferred output falls back to keyboard focus")
}

// forgetHost runs after a screen left byID: removed is the old host.
func TestForgetHostPrefersConfiguredOutputThenFocusThenLowest(t *testing.T) {
	removed := func() *screen { return &screen{output: 10, name: "GONE-1"} }
	hdmi := &screen{output: 49, name: "HDMI-A-1"}
	dp := &screen{output: 50, name: "DP-2"}
	usb := &screen{output: 51, name: "DP-3"}
	all := func() map[neferclient.SurfaceID]*screen {
		return map[neferclient.SurfaceID]*screen{1: hdmi, 2: dp, 3: usb}
	}

	l := hostLocker("DP-2", all())
	l.host = removed()
	l.focus = usb
	l.forgetHost()
	require.Same(t, dp, l.host, "the preferred output first, even over focus")

	l = hostLocker("", all())
	l.host = removed()
	l.focus = usb
	l.forgetHost()
	require.Same(t, usb, l.host, "then the focused screen")

	l = hostLocker("", all())
	l.host = removed()
	l.forgetHost()
	require.Same(t, hdmi, l.host, "then the lowest output")
}

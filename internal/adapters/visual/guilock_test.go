package visual

import (
	"testing"
	"time"

	"bnema.dev/neferclient"
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

func TestCapsLockReachesLockState(t *testing.T) {
	hdmi := &screen{output: 49, name: "HDMI-A-1"}
	l := boundLocker("", map[neferclient.SurfaceID]*screen{1: hdmi})
	l.secret = neferclient.NewSecretBuffer(lockSecretMax)
	now := time.Now()
	require.False(t, l.state(now).Caps)
	l.ModifiersChanged(neferclient.ModCapsLock | neferclient.ModNumLock)
	require.True(t, l.state(now).Caps)
	l.ModifiersChanged(neferclient.ModNumLock)
	require.False(t, l.state(now).Caps)
	l.ModifiersChanged(neferclient.ModCapsLock)
	l.ModifiersChanged(0) // keyboard dropped
	require.False(t, l.state(now).Caps)
}

func TestFailuresCountOnlyRejectedAttempts(t *testing.T) {
	l := boundLocker("", map[neferclient.SurfaceID]*screen{})
	l.secret = neferclient.NewSecretBuffer(lockSecretMax)
	ctl := newAuthController(testLog(), nil, boot(), workerTimeout)
	l.cfg = newLockConfig(testLog(), ctl, make(chan struct{}, 1))
	submit := func() { // the real wiring: OnSubmit queues a copy for the controller
		l.submit()
		select {
		case s := <-ctl.submits:
			clear(s)
		default:
		}
	}
	l.setStatus(lockFailed) // worker loss without a submission
	require.Zero(t, l.fails)
	submit()
	l.setStatus(lockFailed) // fast rejection: lockBusy coalesced away
	require.Equal(t, 1, l.fails)
	l.setStatus(lockFailed) // no new submission
	require.Equal(t, 1, l.fails)
	submit()
	l.setStatus(lockBusy)
	l.setStatus(lockFailed)
	require.Equal(t, 2, l.fails)
	submit()
	l.setStatus(lockIdle) // the attempt ended without a rejection
	l.setStatus(lockFailed)
	require.Equal(t, 2, l.fails)

	submit()
	l.refuseOversized() // an attempt is pending: not counted twice
	l.setStatus(lockFailed)
	require.Equal(t, 3, l.fails)
	l.refuseOversized() // nothing pending: counted
	require.Equal(t, 4, l.fails)
	require.Equal(t, lockFailed, l.status)
}

func TestLockedRecordsLockTime(t *testing.T) {
	l := boundLocker("", map[neferclient.SurfaceID]*screen{})
	l.secret = neferclient.NewSecretBuffer(lockSecretMax)
	require.True(t, l.state(time.Now()).LockedAt.IsZero())
	before := time.Now()
	l.Locked()
	require.False(t, l.state(time.Now()).LockedAt.Before(before))
}

func TestPowerHelpNeedsCtrlAlonePowerActionsAndLock(t *testing.T) {
	l := boundLocker("", map[neferclient.SurfaceID]*screen{})
	l.secret = neferclient.NewSecretBuffer(lockSecretMax)
	now := time.Now()
	l.ModifiersChanged(neferclient.ModCtrl)
	require.False(t, l.state(now).PowerHelp, "power actions off")
	l.cfg.PowerActions = true
	require.False(t, l.state(now).PowerHelp, "not locked yet")
	l.Locked()
	require.True(t, l.state(now).PowerHelp)
	l.ModifiersChanged(neferclient.ModCtrl | neferclient.ModShift)
	require.False(t, l.state(now).PowerHelp, "Ctrl+Shift")
	l.ModifiersChanged(neferclient.ModCtrl | neferclient.ModCapsLock)
	require.True(t, l.state(now).PowerHelp, "Caps Lock does not matter")
	l.ModifiersChanged(0) // released, or keyboard dropped
	require.False(t, l.state(now).PowerHelp)
}

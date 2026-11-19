package visual

import (
	"testing"
	"testing/synctest"
	"time"

	"bnema.dev/neferafk/internal/adapters/visualproc"
	"bnema.dev/neferafk/internal/ports"
	"bnema.dev/neferclient"
	"github.com/stretchr/testify/require"
)

func press(sym uint32, mods neferclient.Modifiers) *neferclient.KeyEvent {
	return &neferclient.KeyEvent{Keysym: sym, Modifiers: mods, Pressed: true}
}

func release(sym uint32, mods neferclient.Modifiers) *neferclient.KeyEvent {
	return &neferclient.KeyEvent{Keysym: sym, Modifiers: mods}
}

// newHold returns a hold that records fired actions.
func newHold() (*powerHold, *[]ports.PowerAction) {
	fired := &[]ports.PowerAction{}
	return &powerHold{fire: func(a ports.PowerAction) { *fired = append(*fired, a) }}, fired
}

// runHold waits for hold ticks for d, advancing like the lock loop.
func runHold(h *powerHold, d time.Duration) {
	end := time.Now().Add(d)
	for h.tick() != nil && time.Now().Before(end) {
		<-h.tick()
		h.advance(time.Now())
	}
	if h.tick() == nil {
		return
	}
	time.Sleep(time.Until(end))
}

func TestPowerSuspendFiresOnPress(t *testing.T) {
	h, fired := newHold()
	require.True(t, h.keyEvent(press(keysymF1, neferclient.ModCtrl), true))
	rep := press(keysymF1, neferclient.ModCtrl)
	rep.Repeat = true
	h.keyEvent(rep, true)
	require.Equal(t, []ports.PowerAction{ports.PowerSuspend}, *fired)
	require.Zero(t, h.progress(time.Now()))
}

func TestPowerShortcutsNeedExactlyCtrl(t *testing.T) {
	h, fired := newHold()
	for _, m := range []neferclient.Modifiers{0, neferclient.ModCtrl | neferclient.ModAlt,
		neferclient.ModCtrl | neferclient.ModShift, neferclient.ModCtrl | neferclient.ModSuper} {
		h.keyEvent(press(keysymF1, m), true)
		h.keyEvent(press(keysymF2, m), true)
	}
	require.Empty(t, *fired)
	require.Zero(t, h.key)
	// Caps Lock is not a blocking modifier.
	h.keyEvent(press(keysymF1, neferclient.ModCtrl|neferclient.ModCapsLock), true)
	require.Equal(t, []ports.PowerAction{ports.PowerSuspend}, *fired)
}

func TestPowerRebootFiresOnlyAfterFullHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, fired := newHold()
		h.keyEvent(press(keysymF2, neferclient.ModCtrl), true)
		rep := press(keysymF2, neferclient.ModCtrl)
		rep.Repeat = true
		require.False(t, h.keyEvent(rep, true), "repeats are ignored")
		require.Greater(t, h.progress(time.Now()), 0.0)
		runHold(h, powerHoldTime/2)
		require.InDelta(t, 0.5, h.progress(time.Now()), 0.05)
		require.Empty(t, *fired)
		runHold(h, powerHoldTime)
		require.Equal(t, []ports.PowerAction{ports.PowerReboot}, *fired)
		require.Zero(t, h.progress(time.Now()))
		require.Nil(t, h.tick())
		// Still pressed after firing: nothing more until a new press.
		h.keyEvent(rep, true)
		h.keyEvent(release(keysymF2, neferclient.ModCtrl), true)
		require.Len(t, *fired, 1)
	})
}

func TestPowerHoldCancels(t *testing.T) {
	for name, ev := range map[string]*neferclient.KeyEvent{
		"F release":    release(keysymF3, neferclient.ModCtrl),
		"Ctrl release": release(keysymControlL, neferclient.ModCtrl),
		"Ctrl gone":    release(0xffe1, 0),
		"other key":    press(0x61, neferclient.ModCtrl),
		"secret key":   {Secret: true, Pressed: true, Modifiers: neferclient.ModCtrl},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, fired := newHold()
				h.keyEvent(press(keysymF3, neferclient.ModCtrl), true)
				runHold(h, powerHoldTime/2)
				require.True(t, h.keyEvent(ev, true))
				require.Zero(t, h.progress(time.Now()))
				runHold(h, 2*powerHoldTime)
				require.Empty(t, *fired)
			})
		})
	}
}

func TestPowerHoldCancelsOnFocusLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, fired := newHold()
		l := &locker{screens: &screens{}, power: *h}
		l.power.keyEvent(press(keysymF3, neferclient.ModCtrl), true)
		l.KeyboardFocus(1, true)
		require.NotZero(t, l.power.key)
		l.KeyboardFocus(1, false)
		require.Zero(t, l.power.progress(time.Now()))
		runHold(&l.power, 2*powerHoldTime)
		require.Empty(t, *fired)
	})
}

func TestPowerShortcutsOffWhenDisabledOrNotLocked(t *testing.T) {
	disabled := &powerHold{} // PowerActions false: no fire callback
	disabled.keyEvent(press(keysymF2, neferclient.ModCtrl), true)
	require.Zero(t, disabled.progress(time.Now()))

	h, fired := newHold()
	h.keyEvent(press(keysymF1, neferclient.ModCtrl), false)
	h.keyEvent(press(keysymF3, neferclient.ModCtrl), false)
	require.Empty(t, *fired)
	require.Zero(t, h.progress(time.Now()))
}

func TestSessionForwardsPowerRequestWithLockGeneration(t *testing.T) {
	h := newHarness(t)
	w := expectWorker(t, h.l)
	require.NoError(t, visualproc.WriteCommand(h.cmdW, ports.VisualLock{Generation: gen, Auth: boot(), PowerActions: true}))
	cfg := recv(t, h.cfgs)
	require.True(t, cfg.PowerActions)
	w.bootstrap(t)
	w.ready(t)
	cfg.OnPower(ports.PowerOff)
	require.Equal(t, ports.PowerRequested{Generation: gen, Action: ports.PowerOff}, h.event(t))
}

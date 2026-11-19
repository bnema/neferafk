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

// newHold returns a hold whose fired actions arrive on a buffered channel.
func newHold() (*powerHold, chan ports.PowerAction) {
	fired := make(chan ports.PowerAction, 8)
	return &powerHold{fire: func(a ports.PowerAction) { fired <- a }}, fired
}

// drained returns the actions fired so far.
func drained(c chan ports.PowerAction) []ports.PowerAction {
	var out []ports.PowerAction
	for {
		select {
		case a := <-c:
			out = append(out, a)
		default:
			return out
		}
	}
}

// runHold serves a running hold's ticks for d, advancing like the lock loop.
func runHold(h *powerHold, d time.Duration) {
	for end := time.Now().Add(d); h.tick() != nil && time.Now().Before(end); {
		<-h.tick()
		h.advance(time.Now())
	}
}

// requireNeverFires lets well over a hold elapse after a cancel and checks
// that advancing still fires nothing.
func requireNeverFires(t *testing.T, h *powerHold, fired chan ports.PowerAction) {
	t.Helper()
	require.Nil(t, h.tick())
	time.Sleep(2 * powerHoldTime)
	synctest.Wait()
	h.advance(time.Now())
	require.Empty(t, drained(fired))
}

func TestPowerSuspendFiresOnPress(t *testing.T) {
	h, fired := newHold()
	require.True(t, h.keyEvent(press(keysymF1, neferclient.ModCtrl), true))
	rep := press(keysymF1, neferclient.ModCtrl)
	rep.Repeat = true
	h.keyEvent(rep, true)
	require.Equal(t, []ports.PowerAction{ports.PowerSuspend}, drained(fired))
	require.Zero(t, h.progress(time.Now()))
}

func TestPowerShortcutsNeedExactlyCtrl(t *testing.T) {
	h, fired := newHold()
	for _, m := range []neferclient.Modifiers{0, neferclient.ModCtrl | neferclient.ModAlt,
		neferclient.ModCtrl | neferclient.ModShift, neferclient.ModCtrl | neferclient.ModSuper} {
		h.keyEvent(press(keysymF1, m), true)
		h.keyEvent(press(keysymF2, m), true)
	}
	require.Empty(t, drained(fired))
	require.Zero(t, h.key)
	// Caps Lock is not a blocking modifier.
	h.keyEvent(press(keysymF1, neferclient.ModCtrl|neferclient.ModCapsLock), true)
	require.Equal(t, []ports.PowerAction{ports.PowerSuspend}, drained(fired))
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
		require.Empty(t, drained(fired))
		runHold(h, powerHoldTime)
		require.Equal(t, []ports.PowerAction{ports.PowerReboot}, drained(fired))
		require.Zero(t, h.progress(time.Now()))
		require.Nil(t, h.tick())
		// Still pressed after firing: nothing more until a new press.
		h.keyEvent(rep, true)
		h.keyEvent(release(keysymF2, neferclient.ModCtrl), true)
		time.Sleep(2 * powerHoldTime)
		h.advance(time.Now())
		require.Empty(t, drained(fired))
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
				requireNeverFires(t, h, fired)
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
		requireNeverFires(t, &l.power, fired)
	})
}

func TestPowerHoldCancelsWhenCtrlLeavesModifiers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, fired := newHold()
		l := &locker{screens: &screens{}, power: *h}
		l.power.keyEvent(press(keysymF2, neferclient.ModCtrl), true)
		l.ModifiersChanged(neferclient.ModCtrl | neferclient.ModCapsLock)
		require.NotZero(t, l.power.key, "Ctrl still held")
		l.ModifiersChanged(0) // Ctrl held at focus: no Control release event
		require.Zero(t, l.power.key)
		requireNeverFires(t, &l.power, fired)
	})
}

func TestPowerHoldSwitchesAndIgnoresUnrelatedReleases(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, fired := newHold()
		h.keyEvent(press(keysymF2, neferclient.ModCtrl), true)
		runHold(h, powerHoldTime/2)
		require.True(t, h.keyEvent(press(keysymF3, neferclient.ModCtrl), true))
		require.Equal(t, ports.PowerOff, h.action)
		require.False(t, h.keyEvent(release(0xffe1, neferclient.ModCtrl), true), "Shift release keeps the hold")
		require.Less(t, h.progress(time.Now()), 0.1, "the switch restarted the hold")
		runHold(h, powerHoldTime)
		require.Equal(t, []ports.PowerAction{ports.PowerOff}, drained(fired))
	})
}

func TestPowerShortcutsOffWhenDisabledOrNotLocked(t *testing.T) {
	disabled := &powerHold{} // PowerActions false: no fire callback
	disabled.keyEvent(press(keysymF2, neferclient.ModCtrl), true)
	require.Zero(t, disabled.progress(time.Now()))

	h, fired := newHold()
	h.keyEvent(press(keysymF1, neferclient.ModCtrl), false)
	h.keyEvent(press(keysymF3, neferclient.ModCtrl), false)
	require.Empty(t, drained(fired))
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

package core

import (
	"fmt"
	"github.com/bnema/neferafk/internal/ports"
	"slices"
	"testing"
	"time"
)

func handle(t *testing.T, p *Policy, now time.Time, e ports.PolicyEvent) []ports.PolicyEffect {
	t.Helper()
	out, err := p.Handle(now, e)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func actions(effects []ports.PolicyEffect) []ports.Action {
	var a []ports.Action
	for _, e := range effects {
		switch e.(type) {
		case ports.AcquireLock:
			a = append(a, ports.Lock)
		case ports.SetFade:
			a = append(a, ports.Fade)
		case ports.SetOutputPower:
			a = append(a, ports.OutputOff)
		case ports.Suspend:
			a = append(a, ports.Sleep)
		}
	}
	return a
}
func config(ds [4]time.Duration) ports.Config {
	c := ports.Defaults()
	c.LockAfter, c.FadeAfter, c.OffAfter, c.SleepAfter = ds[0], ds[1], ds[2], ds[3]
	return c
}

func TestAllSixteenSubsetsTies(t *testing.T) {
	for bits := 0; bits < 16; bits++ {
		t.Run(fmt.Sprint(bits), func(t *testing.T) {
			var ds [4]time.Duration
			var want []ports.Action
			for i := range 4 {
				if bits&(1<<i) != 0 {
					ds[i] = time.Second
					want = append(want, ports.Action(i))
				}
			}
			now := time.Unix(100, 0)
			p, armed, err := New(config(ds), now)
			if err != nil {
				t.Fatal(err)
			}
			arm := armed[0].(ports.ArmIdle)
			if len(arm.Notifications) != min(bits, 1) {
				t.Fatal(arm)
			}
			got := handle(t, p, now.Add(time.Second), ports.IdleDue{Generation: arm.Generation, After: time.Second})
			if bits&1 != 0 {
				got = append(got, handle(t, p, now.Add(time.Second), ports.LockConfirmed{Generation: arm.Generation})...)
			}
			if !slices.Equal(actions(got), want) {
				t.Fatalf("got %v want %v", actions(got), want)
			}
		})
	}
}

func TestAllTwentyFourDeadlineOrders(t *testing.T) {
	var visit func([]int, []int)
	visit = func(prefix, remaining []int) {
		if len(remaining) > 0 {
			for i, v := range remaining {
				visit(append(slices.Clone(prefix), v), append(slices.Clone(remaining[:i]), remaining[i+1:]...))
			}
			return
		}
		t.Run(fmt.Sprint(prefix), func(t *testing.T) {
			var ds [4]time.Duration
			for rank, a := range prefix {
				ds[a] = time.Duration(rank+1) * time.Second
			}
			now := time.Unix(100, 0)
			p, _, _ := New(config(ds), now)
			var got []ports.Action
			for rank := range 4 {
				d := time.Duration(rank+1) * time.Second
				out := handle(t, p, now.Add(d), ports.IdleDue{Generation: 1, After: d})
				got = append(got, actions(out)...)
				if p.acquiring {
					got = append(got, actions(handle(t, p, now.Add(d), ports.LockConfirmed{Generation: p.lockGeneration}))...)
				}
			}
			var want []ports.Action
			for _, a := range prefix {
				want = append(want, ports.Action(a))
			}
			if !slices.Equal(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
		})
	}
	visit(nil, []int{0, 1, 2, 3})
}

func TestAcquiringHoldsOnlyDueOffSleepActivityNeverUnlocks(t *testing.T) {
	now := time.Unix(100, 0)
	p, _, _ := New(config([4]time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second}), now)
	handle(t, p, now, ports.IdleDue{Generation: 1, After: time.Second})
	if len(handle(t, p, now, ports.IdleDue{Generation: 1, After: 3 * time.Second})) != 0 {
		t.Fatal("off before lock confirmation")
	}
	if got := actions(handle(t, p, now, ports.IdleDue{Generation: 1, After: 2 * time.Second})); !slices.Equal(got, []ports.Action{ports.Fade}) {
		t.Fatal(got)
	}
	handle(t, p, now, ports.IdleDue{Generation: 1, After: 4 * time.Second})
	if got := actions(handle(t, p, now, ports.LockConfirmed{Generation: 1})); !slices.Equal(got, []ports.Action{ports.OutputOff, ports.Sleep}) {
		t.Fatal(got)
	}
	out := handle(t, p, now, ports.Activity{})
	if !p.protected || p.off || p.faded {
		t.Fatal(p.State())
	}
	if len(out) != 3 {
		t.Fatal(out)
	}
	if len(handle(t, p, now, ports.IdleDue{Generation: 1, After: time.Second})) != 0 {
		t.Fatal("old proxy event ran")
	}
	handle(t, p, now, ports.LockReleased{Generation: 99})
	if !p.protected {
		t.Fatal("stale unlock")
	}
	handle(t, p, now, ports.LockReleased{Generation: 1})
	if p.protected {
		t.Fatal("valid visual release ignored")
	}
}

func TestReloadRebasesOnlyChangedPendingAndKeepsProtection(t *testing.T) {
	now := time.Unix(100, 0)
	cfg := ports.Defaults()
	p, _, _ := New(cfg, now)
	next := cfg
	next.FadeAfter = 8 * time.Minute
	out := handle(t, p, now.Add(time.Minute), ports.Reload{Config: next})
	arm := out[0].(ports.ArmIdle)
	want := []ports.IdleNotification{{ID: 1, After: 5 * time.Minute}, {ID: 3, After: 7 * time.Minute}, {ID: 5, After: 8 * time.Minute}, {ID: 4, After: 20 * time.Minute}}
	if !slices.Equal(arm.Notifications, want) {
		t.Fatalf("arm %+v", arm)
	}
	handle(t, p, now, ports.IdleDue{Generation: 1, Notification: 1, After: 5 * time.Minute})
	next.LockAfter = 0
	handle(t, p, now, ports.Reload{Config: next})
	if !p.acquiring {
		t.Fatal("reload cancelled acquisition")
	}
	handle(t, p, now, ports.LockConfirmed{Generation: 2})
	if !p.protected {
		t.Fatal("reload lost lock generation")
	}
	bad := next
	bad.SleepAfter = -1
	if _, err := p.Handle(now, ports.Reload{Config: bad}); err == nil {
		t.Fatal("bad config committed")
	}
	if p.cfg != next {
		t.Fatal("partial reload")
	}
	handle(t, p, now, ports.Activity{})
	if !p.protected {
		t.Fatal("activity unlock")
	}
}

func TestBeforeSleepHonorsLockDisabled(t *testing.T) {
	now := time.Unix(100, 0)
	for _, enabled := range []bool{false, true} {
		cfg := ports.Defaults()
		if !enabled {
			cfg.LockAfter = 0
		}
		p, _, _ := New(cfg, now)
		out := handle(t, p, now, ports.BeforeSleep{})
		if !enabled {
			if _, ok := out[0].(ports.SleepReady); !ok {
				t.Fatal(out)
			}
			if p.acquiring {
				t.Fatal("forced disabled lock")
			}
		} else {
			if _, ok := out[0].(ports.AcquireLock); !ok {
				t.Fatal(out)
			}
			out = handle(t, p, now, ports.LockConfirmed{Generation: 1})
			if _, ok := out[len(out)-1].(ports.SleepReady); !ok {
				t.Fatal(out)
			}
		}
	}
}

func TestReloadRetainsUnchangedProxyRejectsReplacedProxyAndDoneFlags(t *testing.T) {
	now := time.Unix(100, 0)
	cfg := ports.Defaults()
	p, initial, _ := New(cfg, now)
	old := initial[0].(ports.ArmIdle)
	fadeID := old.Notifications[1].ID
	next := cfg
	next.FadeAfter = 8 * time.Minute
	out := handle(t, p, now.Add(time.Minute), ports.Reload{Config: next})
	if len(handle(t, p, now, ports.IdleDue{Generation: 1, Notification: fadeID, After: 6 * time.Minute})) != 0 {
		t.Fatal("replaced proxy delivered")
	}
	var newFade ports.IdleNotification
	for _, n := range out[0].(ports.ArmIdle).Notifications {
		if n.After == 8*time.Minute {
			newFade = n
		}
	}
	if got := actions(handle(t, p, now, ports.IdleDue{Generation: 2, Notification: newFade.ID, After: newFade.After})); !slices.Equal(got, []ports.Action{ports.Fade}) {
		t.Fatal(got)
	}
	next.FadeAfter = time.Minute
	handle(t, p, now, ports.Reload{Config: next})
	if !p.deadlines[ports.Fade].done {
		t.Fatal("reload reset completed fade")
	}
	if len(handle(t, p, now, ports.IdleDue{Generation: 2, Notification: newFade.ID, After: newFade.After})) != 0 {
		t.Fatal("done fade replayed")
	}
}

func TestCompletedFadeDisableReenableRetiresOldNotification(t *testing.T) {
	now := time.Unix(100, 0)
	cfg := config([4]time.Duration{0, time.Second, 0, 0})
	p, initial, _ := New(cfg, now)
	proxy := initial[0].(ports.ArmIdle).Notifications[0]
	handle(t, p, now, ports.IdleDue{Generation: 1, Notification: proxy.ID, After: proxy.After})
	cfg.FadeAfter = 0
	handle(t, p, now, ports.Reload{Config: cfg})
	d := p.deadlines[ports.Fade]
	if d.after != 0 || !d.done || d.due || d.notification != 0 {
		t.Fatalf("disable metadata %+v", d)
	}
	cfg.FadeAfter = 2 * time.Second
	out := handle(t, p, now, ports.Reload{Config: cfg})
	if len(out[0].(ports.ArmIdle).Notifications) != 0 {
		t.Fatal("completed action rearmed")
	}
	if len(handle(t, p, now, ports.IdleDue{Generation: 1, Notification: proxy.ID, After: proxy.After})) != 0 {
		t.Fatal("old event replayed")
	}
	handle(t, p, now, ports.Activity{})
	if p.deadlines[ports.Fade].after != 2*time.Second || p.deadlines[ports.Fade].done {
		t.Fatal("next cycle ignored latest enablement")
	}
}

func TestEqualAfterReloadKeepsOriginalProxySeparateFromRebasedDeadline(t *testing.T) {
	now := time.Unix(100, 0)
	cfg := config([4]time.Duration{time.Minute, 2 * time.Minute, 0, 0})
	p, initial, _ := New(cfg, now)
	old := initial[0].(ports.ArmIdle).Notifications[0]
	cfg.FadeAfter = time.Minute
	arm := handle(t, p, now.Add(30*time.Second), ports.Reload{Config: cfg})[0].(ports.ArmIdle)
	if len(arm.Notifications) != 2 || arm.Notifications[0].ID == arm.Notifications[1].ID {
		t.Fatal("unrelated clock origins incorrectly coalesced")
	}
	// Equal timeout values do not imply equal due instants: fade was rearmed
	// at reload while lock's existing notification stayed registered.
	if got := actions(handle(t, p, now, ports.IdleDue{Generation: 1, Notification: old.ID, After: old.After})); !slices.Equal(got, []ports.Action{ports.Lock}) {
		t.Fatal(got)
	}
	if got := actions(handle(t, p, now, ports.IdleDue{Generation: 2, Notification: arm.Notifications[1].ID, After: time.Minute})); !slices.Equal(got, []ports.Action{ports.Fade}) {
		t.Fatal(got)
	}
}

func TestLockRequestedAcquiresOnceAndNeverUnlocks(t *testing.T) {
	now := time.Unix(100, 0)
	p, _, _ := New(config([4]time.Duration{0, 0, time.Minute, 0}), now)
	out := handle(t, p, now, ports.LockRequested{})
	if len(out) != 1 || out[0] != (ports.AcquireLock{Generation: 1}) {
		t.Fatalf("lock request %v", out)
	}
	if got := handle(t, p, now, ports.LockRequested{}); len(got) != 0 {
		t.Fatalf("duplicate acquisition %v", got)
	}
	handle(t, p, now, ports.LockConfirmed{Generation: 1})
	if got := handle(t, p, now, ports.LockRequested{}); len(got) != 0 || !p.State().Protected {
		t.Fatalf("protected lock re-requested %v", got)
	}
	handle(t, p, now, ports.Activity{})
	if !p.State().Protected {
		t.Fatal("activity released the lock")
	}
}

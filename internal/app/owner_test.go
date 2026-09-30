package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	portsmocks "github.com/bnema/neferafk/internal/mocks/ports"
	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/zerowrap"
	"github.com/stretchr/testify/mock"
)

const wait = 2 * time.Second

type harness struct {
	t      *testing.T
	visual *portsmocks.MockVisual
	cfgCh  chan ports.ConfigChanged
	fail   chan error
	wlEv   chan ports.WaylandEvent
	sysEv  chan ports.SystemEvent
	visEv  chan ports.VisualEvent
	ctl    chan ports.ControlRequest
	wlCmd  chan ports.WaylandCommand
	sysCmd chan ports.SystemCommand
	done   chan error
	cancel context.CancelFunc
	owner  *Owner
}

// newHarness starts an owner. Like the real Wayland adapter, non-gate harnesses
// report one seat (0) before any idle event.
func newHarness(t *testing.T, cfg ports.Config, tweak func(*Options)) *harness {
	t.Helper()
	h, gate := newBareHarness(t, cfg, tweak)
	if !gate {
		h.send(ports.WaylandCapabilities{Capabilities: ports.Capabilities{Seats: []uint32{0}}})
	}
	return h
}

// newBareHarness starts an owner without any capability report.
func newBareHarness(t *testing.T, cfg ports.Config, tweak func(*Options)) (*harness, bool) {
	t.Helper()
	h := &harness{t: t, visual: portsmocks.NewMockVisual(t),
		cfgCh: make(chan ports.ConfigChanged), fail: make(chan error, 4), wlEv: make(chan ports.WaylandEvent), sysEv: make(chan ports.SystemEvent),
		visEv: make(chan ports.VisualEvent), ctl: make(chan ports.ControlRequest), wlCmd: make(chan ports.WaylandCommand, 32), sysCmd: make(chan ports.SystemCommand, 32), done: make(chan error, 1)}
	opt := Options{Config: cfg, Visual: h.visual, Log: zerowrap.New(zerowrap.Config{Output: io.Discard}), RetryMin: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond}
	if tweak != nil {
		tweak(&opt)
	}
	o, err := NewOwner(opt, Inputs{Config: h.cfgCh, Failures: h.fail, Wayland: h.wlEv, System: h.sysEv, Visual: h.visEv, Control: h.ctl}, Outputs{Wayland: h.wlCmd, System: h.sysCmd})
	if err != nil {
		t.Fatal(err)
	}
	h.owner = o
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- o.Run(ctx) }()
	t.Cleanup(func() { h.stop() })
	return h, opt.Gate
}

// stop ends the owner (once) and returns its error.
func (h *harness) stop() error {
	h.cancel()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(wait):
		h.t.Fatal("owner did not stop")
		return nil
	}
}

// result waits for the owner to exit on its own (without cancelling it).
func (h *harness) result() error {
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(wait):
		h.t.Fatal("owner did not exit")
		return nil
	}
}

func (h *harness) deadlines() ports.SetDeadlines {
	h.t.Helper()
	for {
		select {
		case c := <-h.wlCmd:
			if d, ok := c.(ports.SetDeadlines); ok {
				return d
			}
		case <-time.After(wait):
			h.t.Fatal("no SetDeadlines")
		}
	}
}
func (h *harness) wlCommand() ports.WaylandCommand {
	h.t.Helper()
	select {
	case c := <-h.wlCmd:
		return c
	case <-time.After(wait):
		h.t.Fatal("no wayland command")
		return nil
	}
}
func (h *harness) noWlCommand() {
	h.t.Helper()
	select {
	case c := <-h.wlCmd:
		h.t.Fatalf("unexpected wayland command %#v", c)
	case <-time.After(60 * time.Millisecond):
	}
}
func (h *harness) sysCommand() ports.SystemCommand {
	h.t.Helper()
	select {
	case c := <-h.sysCmd:
		return c
	case <-time.After(wait):
		h.t.Fatal("no system command")
		return nil
	}
}
func (h *harness) noSysCommand() {
	h.t.Helper()
	select {
	case c := <-h.sysCmd:
		h.t.Fatalf("unexpected system command %#v", c)
	case <-time.After(60 * time.Millisecond):
	}
}
func (h *harness) send(ev ports.WaylandEvent) {
	h.t.Helper()
	select {
	case h.wlEv <- ev:
	case <-time.After(wait):
		h.t.Fatal("owner stuck")
	}
}
func (h *harness) status() ports.ControlReply {
	h.t.Helper()
	reply := make(chan ports.ControlReply, 1)
	select {
	case h.ctl <- ports.ControlRequest{Kind: ports.ControlStatus, Reply: reply}:
	case <-time.After(wait):
		h.t.Fatal("owner stuck")
	}
	return <-reply
}

func cfgOf(lock, fade, off, sleep time.Duration) ports.Config {
	c := ports.Defaults()
	c.LockAfter, c.FadeAfter, c.OffAfter, c.SleepAfter = lock, fade, off, sleep
	c.FadeDuration = time.Second
	return c
}

const step = 30 * time.Millisecond

// lockCapture makes Visual.Lock record its request.
func (h *harness) lockCapture() <-chan ports.VisualLock {
	locks := make(chan ports.VisualLock, 8)
	h.visual.EXPECT().Lock(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, r ports.VisualLock) error {
		locks <- r
		return nil
	})
	return locks
}
func lockOf(t *testing.T, ch <-chan ports.VisualLock) ports.VisualLock {
	t.Helper()
	select {
	case l := <-ch:
		return l
	case <-time.After(wait):
		t.Fatal("no lock request")
		return ports.VisualLock{}
	}
}

func TestIdleActionsAreIndependentAndDisabledOnesNeverFire(t *testing.T) {
	// Only fade and output-off are enabled; lock and sleep are "off".
	h := newHarness(t, cfgOf(0, step, 2*step, 0), nil)
	fades := make(chan ports.VisualFade, 4)
	h.visual.EXPECT().Fade(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, f ports.VisualFade) error { fades <- f; return nil })
	h.visual.EXPECT().Close().Return(nil).Once()
	d := h.deadlines()
	if len(d.Deadlines) != 2 || d.Deadlines[0] != step || d.Deadlines[1] != 2*step {
		t.Fatalf("deadlines %v", d.Deadlines)
	}
	h.send(ports.WaylandIdle{After: step})
	if f := <-fades; !f.Black || f.Duration != time.Second {
		t.Fatalf("fade %+v", f)
	}
	h.send(ports.WaylandIdle{After: 2 * step})
	if c := h.wlCommand(); c != (ports.OutputPower{On: false}) {
		t.Fatalf("command %#v", c)
	}
	h.noSysCommand() // sleep is off: nothing suspends
	if st := h.status(); st.Acquiring || st.Protected {
		t.Fatalf("locked without lock.after: %+v", st)
	}
}

func TestActivityUndoesFadeWakesOutputsButNeverUnlocks(t *testing.T) {
	h := newHarness(t, cfgOf(step, 2*step, 3*step, 0), nil)
	locks := h.lockCapture()
	fades := make(chan ports.VisualFade, 4)
	h.visual.EXPECT().Fade(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, f ports.VisualFade) error { fades <- f; return nil })
	h.visual.EXPECT().Detach().Return(nil).Once() // shutdown with a held lock detaches, never closes
	d := h.deadlines()
	for _, after := range d.Deadlines {
		h.send(ports.WaylandIdle{After: after})
	}
	l := lockOf(t, locks)
	if l.Generation != 1 || l.Auth.Generation != 1 {
		t.Fatalf("lock %+v", l)
	}
	h.visEv <- ports.LockConfirmed{Generation: l.Generation}
	if f := <-fades; !f.Black {
		t.Fatal("fade not started")
	}
	if c := h.wlCommand(); c != (ports.OutputPower{On: false}) {
		t.Fatalf("command %#v", c)
	}
	h.send(ports.WaylandActivity{})
	if f := <-fades; f.Black {
		t.Fatal("fade not reversed")
	}
	if c := h.wlCommand(); c != (ports.OutputPower{On: true}) {
		t.Fatalf("command %#v", c)
	}
	st := h.status()
	if !st.Protected || st.Faded || st.Off {
		t.Fatalf("status %+v", st)
	}
	// Repeated activity, logind Unlock, and a reload: the lock stays held.
	h.send(ports.WaylandActivity{})
	h.sysEv <- ports.SystemUnlockObserved{}
	h.cfgCh <- ports.ConfigChanged{Config: cfgOf(step, 0, 0, 0)}
	if st := h.status(); !st.Protected {
		t.Fatalf("lock released: %+v", st)
	}
	h.stop()
}

func TestLockIsNeverForcedOrReordered(t *testing.T) {
	// Fade only: no lock is acquired (mock has no Lock expectation).
	h := newHarness(t, cfgOf(0, step, 0, 0), nil)
	h.visual.EXPECT().Fade(mock.Anything, mock.Anything).Return(nil)
	h.visual.EXPECT().Close().Return(nil).Once()
	d := h.deadlines()
	h.send(ports.WaylandIdle{After: d.Deadlines[0]})
	h.status()
	// Off and sleep share the lock's deadline: they wait for confirmation.
	h2 := newHarness(t, cfgOf(step, 0, step, step), nil)
	locks := h2.lockCapture()
	h2.visual.EXPECT().Detach().Return(nil).Once()
	d2 := h2.deadlines()
	if len(d2.Deadlines) != 1 {
		t.Fatalf("deadlines %v", d2.Deadlines)
	}
	h2.send(ports.WaylandIdle{After: step})
	l := lockOf(t, locks)
	h2.noWlCommand()
	h2.noSysCommand()
	h2.visEv <- ports.LockConfirmed{Generation: l.Generation}
	if c := h2.wlCommand(); c != (ports.OutputPower{On: false}) {
		t.Fatalf("command %#v", c)
	}
	if c := h2.sysCommand(); c != (ports.SystemSuspend{}) {
		t.Fatalf("command %#v", c)
	}
}

func TestReloadRearmsChangedDeadlinesAndRejectedConfigKeepsState(t *testing.T) {
	h := newHarness(t, cfgOf(0, step, 0, 0), nil)
	h.visual.EXPECT().Close().Return(nil).Once()
	if d := h.deadlines(); len(d.Deadlines) != 1 || d.Deadlines[0] != step {
		t.Fatalf("%v", d.Deadlines)
	}
	h.cfgCh <- ports.ConfigChanged{Config: cfgOf(0, 3*step, 0, 0)}
	d := h.deadlines()
	if d.Generation != 2 || len(d.Deadlines) != 1 || d.Deadlines[0] != 3*step {
		t.Fatalf("reload %+v", d)
	}
	// Identical config: no new arming. Invalid config: rejected, state kept.
	h.cfgCh <- ports.ConfigChanged{Config: cfgOf(0, 3*step, 0, 0)}
	bad := cfgOf(0, 3*step, 0, 0)
	bad.LockAfter = -1
	h.cfgCh <- ports.ConfigChanged{Config: bad}
	h.fail <- errors.New("config reload rejected")
	h.status()
	h.noWlCommand()
	// Disabling everything clears the deadlines.
	h.cfgCh <- ports.ConfigChanged{Config: cfgOf(0, 0, 0, 0)}
	if d := h.deadlines(); len(d.Deadlines) != 0 {
		t.Fatalf("still armed: %v", d.Deadlines)
	}
}

func TestEqualDurationReloadDoesNotFireEarly(t *testing.T) {
	// Lock (enabled) and fade share an idle duration only after the reload;
	// the fade registration is new and must wait its own full duration even
	// though the adapter's proxy for that duration is already idle.
	after := 120 * time.Millisecond
	h := newHarness(t, cfgOf(after, 0, 0, 0), nil)
	locks := h.lockCapture()
	fades := make(chan ports.VisualFade, 4)
	h.visual.EXPECT().Fade(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, f ports.VisualFade) error { fades <- f; return nil })
	h.visual.EXPECT().Detach().Return(nil).Once()
	h.deadlines()
	time.Sleep(after / 2)
	h.cfgCh <- ports.ConfigChanged{Config: cfgOf(after, after, 0, 0)}
	h.deadlines()
	start := time.Now()
	h.send(ports.WaylandIdle{After: after}) // proxy idles at the original origin
	lockOf(t, locks)
	select {
	case <-fades:
		if time.Since(start) < after/2-10*time.Millisecond {
			t.Fatal("fade fired before its own deadline")
		}
	case <-time.After(wait):
		t.Fatal("fade never fired")
	}
}

func TestSleepReadyOnlyAfterLockConfirmed(t *testing.T) {
	h := newHarness(t, cfgOf(time.Hour, 0, 0, 0), nil)
	locks := h.lockCapture()
	h.visual.EXPECT().Detach().Return(nil).Once()
	h.deadlines()
	h.sysEv <- ports.SleepPreparation{Preparing: true, Cycle: 7}
	l := lockOf(t, locks) // acquiring the lock is what BeforeSleep asks for
	h.noSysCommand()      // ...and the inhibitor is held until it is confirmed
	h.visEv <- ports.LockConfirmed{Generation: l.Generation}
	if c := h.sysCommand(); c != (ports.SystemSleepReady{Cycle: 7}) {
		t.Fatalf("command %#v", c)
	}
	// Already protected: the next cycle is released immediately.
	h.sysEv <- ports.SleepPreparation{Preparing: false, Cycle: 7}
	h.sysEv <- ports.SleepPreparation{Preparing: true, Cycle: 8}
	if c := h.sysCommand(); c != (ports.SystemSleepReady{Cycle: 8}) {
		t.Fatalf("command %#v", c)
	}
	h.noSysCommand()
}

func TestSleepWithLockDisabledIsReleasedWithoutLocking(t *testing.T) {
	h := newHarness(t, cfgOf(0, 0, 0, time.Hour), nil) // no Lock expectation
	h.visual.EXPECT().Close().Return(nil).Once()
	h.deadlines()
	h.sysEv <- ports.SleepPreparation{Preparing: true, Cycle: 1}
	if c := h.sysCommand(); c != (ports.SystemSleepReady{Cycle: 1}) {
		t.Fatalf("command %#v", c)
	}
}

func TestLogindLockRequestAcquiresAndControlLockIsIdempotent(t *testing.T) {
	h := newHarness(t, cfgOf(0, 0, 0, 0), nil)
	locks := h.lockCapture()
	h.visual.EXPECT().Detach().Return(nil).Once()
	h.deadlines()
	h.sysEv <- ports.SystemLockRequested{}
	lockOf(t, locks)
	reply := make(chan ports.ControlReply, 1)
	h.ctl <- ports.ControlRequest{Kind: ports.ControlLock, Reply: reply}
	if r := <-reply; !r.Acquiring || r.Err != "" {
		t.Fatalf("reply %+v", r)
	}
	select {
	case <-locks:
		t.Fatal("second acquisition")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestControlUnknownKindRejected(t *testing.T) {
	h := newHarness(t, cfgOf(0, 0, 0, 0), nil)
	h.visual.EXPECT().Close().Return(nil).Once()
	reply := make(chan ports.ControlReply, 1)
	h.ctl <- ports.ControlRequest{Kind: ports.ControlKind(99), Reply: reply}
	if r := <-reply; r.Err == "" {
		t.Fatal("unknown request accepted")
	}
}

func TestVisualCrashKeepsLockAndRespawnsWithBackoff(t *testing.T) {
	h := newHarness(t, cfgOf(0, 0, 0, 0), nil)
	locks := make(chan ports.VisualLock, 8)
	calls := 0
	h.visual.EXPECT().Lock(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, r ports.VisualLock) error {
		calls++
		locks <- r
		if calls == 2 {
			return errors.New("spawn failed") // first respawn attempt fails, retried
		}
		return nil
	})
	h.visual.EXPECT().Detach().Return(nil).Once()
	h.deadlines()
	h.sysEv <- ports.SystemLockRequested{}
	l := lockOf(t, locks)
	h.visEv <- ports.LockConfirmed{Generation: l.Generation}
	h.visEv <- ports.VisualExited{Err: errors.New("crash")}
	for i := 0; i < 2; i++ {
		if again := lockOf(t, locks); again.Generation != l.Generation {
			t.Fatalf("respawn generation %d", again.Generation)
		}
	}
	if st := h.status(); !st.Protected {
		t.Fatalf("crash released the lock: %+v", st)
	}
}

func TestVisualExitAfterReleaseDoesNotRespawn(t *testing.T) {
	h := newHarness(t, cfgOf(0, 0, 0, 0), nil)
	locks := h.lockCapture()
	h.visual.EXPECT().Close().Return(nil).Once()
	h.deadlines()
	h.sysEv <- ports.SystemLockRequested{}
	l := lockOf(t, locks)
	h.visEv <- ports.LockConfirmed{Generation: l.Generation}
	h.visEv <- ports.LockReleased{Generation: l.Generation}
	h.visEv <- ports.VisualExited{}
	if st := h.status(); st.Protected || st.Acquiring {
		t.Fatalf("status %+v", st)
	}
	select {
	case <-locks:
		t.Fatal("respawned after successful unlock")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestLockBootstrapCarriesMetadataAndEnvPINStaysOutOfChildEnvironment(t *testing.T) {
	cfg := cfgOf(0, 0, 0, 0)
	cfg.Auth = ports.AuthConfig{Mode: ports.AuthModePIN, PINSource: ports.PINSourceEnv, PINEnv: "NEFER_PIN"}
	h := newHarness(t, cfg, func(o *Options) {
		o.LookupEnv = func(k string) (string, bool) { return "123456", k == "NEFER_PIN" }
	})
	locks := h.lockCapture()
	h.visual.EXPECT().Detach().Return(nil).Once()
	h.deadlines()
	h.sysEv <- ports.SystemLockRequested{}
	l := lockOf(t, locks)
	if l.Auth.PINSource != ports.PINSourceEnv || string(l.Auth.EnvPIN) != "123456" || len(l.Spawn.OmitEnv) != 1 || l.Spawn.OmitEnv[0] != "NEFER_PIN" {
		t.Fatalf("lock %+v", l)
	}
	// Pass source: only the entry name travels, never a value.
	cfg.Auth = ports.AuthConfig{Mode: ports.AuthModePIN, PINSource: ports.PINSourcePass, PINEntry: "afk/pin"}
	h.cfgCh <- ports.ConfigChanged{Config: cfg}
}

func TestStartupGate(t *testing.T) {
	ok := ports.Capabilities{}
	for _, tc := range []struct {
		name    string
		wl      ports.Capabilities
		sys     ports.SystemCapabilities
		wantErr bool
	}{
		{"supported", ok, ports.SystemCapabilities{Session: true, Suspend: true}, false},
		{"wayland missing", ports.Capabilities{Missing: []string{"ext_idle_notifier_v1 >= 2"}}, ports.SystemCapabilities{Session: true, Suspend: true}, true},
		{"no session", ok, ports.SystemCapabilities{}, true},
		{"suspend unavailable but sleep off", ok, ports.SystemCapabilities{Session: true, Missing: []string{"non-interactive suspend: no"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, cfgOf(0, 0, 0, 0), func(o *Options) { o.Gate, o.ExpectSystem, o.GateTimeout = true, true, 100*time.Millisecond })
			h.visual.EXPECT().Close().Return(nil).Maybe()
			h.send(ports.WaylandCapabilities{Capabilities: tc.wl})
			h.sysEv <- ports.SystemCapabilityReport{Capabilities: tc.sys}
			if tc.wantErr {
				var u *UnsupportedError
				if err := h.result(); !errors.As(err, &u) || len(u.Missing) == 0 {
					t.Fatalf("err %v", err)
				}
				return
			}
			h.status()
		})
	}
}

func TestStartupGateWaitsForLateWaylandGlobals(t *testing.T) {
	h := newHarness(t, cfgOf(0, 0, 0, 0), func(o *Options) {
		o.Gate, o.ExpectSystem, o.GateTimeout = true, true, 80*time.Millisecond
	})
	h.visual.EXPECT().Close().Return(nil).Maybe()
	h.send(ports.WaylandCapabilities{Capabilities: ports.Capabilities{Missing: []string{"wl_output >= 1"}}})
	h.sysEv <- ports.SystemCapabilityReport{Capabilities: ports.SystemCapabilities{Session: true, Suspend: true}}
	h.send(ports.WaylandCapabilities{})
	time.Sleep(3 * 80 * time.Millisecond)
	select {
	case err := <-h.done:
		t.Fatalf("owner exited although the late report was complete: %v", err)
	default:
	}
	h.status()
}

func TestStartupGateFailsWhenWaylandStaysIncomplete(t *testing.T) {
	h := newHarness(t, cfgOf(0, 0, 0, 0), func(o *Options) {
		o.Gate, o.ExpectSystem, o.GateTimeout = true, true, 50*time.Millisecond
	})
	h.visual.EXPECT().Close().Return(nil).Maybe()
	h.send(ports.WaylandCapabilities{Capabilities: ports.Capabilities{Missing: []string{"wl_output >= 1"}}})
	h.sysEv <- ports.SystemCapabilityReport{Capabilities: ports.SystemCapabilities{Session: true, Suspend: true}}
	var u *UnsupportedError
	if err := h.result(); !errors.As(err, &u) || len(u.Missing) != 1 || u.Missing[0] != "wayland: wl_output >= 1" {
		t.Fatalf("err %v", err)
	}
}

func TestStartupGateFailsAtOnceWhenSystemMissing(t *testing.T) {
	h := newHarness(t, cfgOf(0, 0, 0, 0), func(o *Options) { o.Gate, o.ExpectSystem, o.GateTimeout = true, true, time.Hour })
	h.visual.EXPECT().Close().Return(nil).Maybe()
	h.send(ports.WaylandCapabilities{Capabilities: ports.Capabilities{Missing: []string{"wl_output >= 1"}}})
	h.sysEv <- ports.SystemCapabilityReport{Capabilities: ports.SystemCapabilities{}}
	var u *UnsupportedError
	if err := h.result(); !errors.As(err, &u) || len(u.Missing) != 2 {
		t.Fatalf("err %v", err)
	}
}

// lockedBuffer is a real io.Writer shared by the owner and the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestStartupGateSuspendUnavailableDoesNotBlockOtherActions(t *testing.T) {
	logs := &lockedBuffer{}
	h := newHarness(t, cfgOf(0, 0, 0, time.Hour), func(o *Options) {
		o.Gate, o.ExpectSystem = true, true
		o.Log = zerowrap.New(zerowrap.Config{Output: logs})
	})
	h.visual.EXPECT().Close().Return(nil).Maybe()
	h.send(ports.WaylandCapabilities{})
	h.sysEv <- ports.SystemCapabilityReport{Capabilities: ports.SystemCapabilities{Session: true, SuspendAuthorization: "inhibited", Missing: []string{"non-interactive suspend: inhibited"}}}
	h.status() // still running: only the sleep action is degraded
	if !strings.Contains(logs.String(), "suspend unavailable (inhibited)") {
		t.Fatalf("missing suspend warning in %q", logs.String())
	}
}

func TestStartupGateSleepInhibitorStillRequired(t *testing.T) {
	h := newHarness(t, cfgOf(0, 0, 0, time.Hour), func(o *Options) { o.Gate, o.ExpectSystem = true, true })
	h.visual.EXPECT().Close().Return(nil).Maybe()
	h.send(ports.WaylandCapabilities{})
	h.sysEv <- ports.SystemCapabilityReport{Capabilities: ports.SystemCapabilities{Session: true, Missing: []string{"sleep delay inhibitor"}}}
	var u *UnsupportedError
	if err := h.result(); !errors.As(err, &u) {
		t.Fatalf("err %v", err)
	}
}

func TestNewOwnerValidation(t *testing.T) {
	if _, err := NewOwner(Options{Config: ports.Defaults()}, Inputs{}, Outputs{}); err == nil {
		t.Fatal("nil visual accepted")
	}
	bad := ports.Defaults()
	bad.LockAfter = -1
	if _, err := NewOwner(Options{Config: bad, Visual: portsmocks.NewMockVisual(t)}, Inputs{}, Outputs{}); err == nil {
		t.Fatal("invalid config accepted")
	}
}

func TestGatePassedSurvivesPastGateTimeout(t *testing.T) {
	h := newHarness(t, cfgOf(0, 0, 0, 0), func(o *Options) {
		o.Gate, o.ExpectSystem, o.GateTimeout = true, true, 80*time.Millisecond
	})
	h.visual.EXPECT().Close().Return(nil).Maybe()
	h.send(ports.WaylandCapabilities{})
	h.sysEv <- ports.SystemCapabilityReport{Capabilities: ports.SystemCapabilities{Session: true, Suspend: true}}
	time.Sleep(3 * 80 * time.Millisecond)
	select {
	case err := <-h.done:
		t.Fatalf("owner exited after the gate passed: %v", err)
	default:
	}
	h.status() // still serving
}

func TestGateTimeoutStillFailsWithoutReports(t *testing.T) {
	h := newHarness(t, cfgOf(0, 0, 0, 0), func(o *Options) { o.Gate, o.GateTimeout = true, 50*time.Millisecond })
	h.visual.EXPECT().Close().Return(nil).Maybe()
	var u *UnsupportedError
	if err := h.result(); !errors.As(err, &u) {
		t.Fatalf("err %v", err)
	}
}

func TestDeadlineNeedsEverySeatIdleAndActivityClearsOnlyItsSeat(t *testing.T) {
	h := newHarness(t, cfgOf(0, step, 0, 0), nil)
	fades := make(chan ports.VisualFade, 4)
	h.visual.EXPECT().Fade(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, f ports.VisualFade) error { fades <- f; return nil })
	h.visual.EXPECT().Close().Return(nil).Once()
	h.deadlines()
	h.send(ports.WaylandCapabilities{Capabilities: ports.Capabilities{Seats: []uint32{1, 2}}})
	h.send(ports.WaylandIdle{Seat: 1, After: step})
	select {
	case <-fades:
		t.Fatal("fade fired with one of two seats idle")
	case <-time.After(3 * step):
	}
	h.send(ports.WaylandIdle{Seat: 2, After: step})
	if f := <-fades; !f.Black {
		t.Fatal("fade not started once all seats idle")
	}
	// Seat 2 becomes active: undo, and only seat 2's idle state is cleared.
	h.send(ports.WaylandActivity{Seat: 2})
	if f := <-fades; f.Black {
		t.Fatal("fade not reversed")
	}
	// Seat 1 stayed idle (its own state was not cleared), so seat 2 idling
	// again completes "all seats idle" and the re-armed deadline fires anew...
	h.send(ports.WaylandIdle{Seat: 2, After: step})
	if f := <-fades; !f.Black {
		t.Fatal("re-armed fade did not fire once seat 2 idled again")
	}
	// ...but activity on seat 1 clears seat 1 only: with seat 1 busy and seat 2
	// idle, nothing may fire.
	h.send(ports.WaylandActivity{Seat: 1})
	if f := <-fades; f.Black {
		t.Fatal("fade not reversed")
	}
	select {
	case f := <-fades:
		t.Fatalf("fired with seat 1 busy: %+v", f)
	case <-time.After(3 * step):
	}
}

func TestNothingFiresBeforeFirstSeatReport(t *testing.T) {
	h, _ := newBareHarness(t, cfgOf(0, step, 0, 0), nil)
	fades := make(chan ports.VisualFade, 4)
	h.visual.EXPECT().Fade(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, f ports.VisualFade) error { fades <- f; return nil })
	h.visual.EXPECT().Close().Return(nil).Once()
	h.deadlines()
	h.send(ports.WaylandIdle{Seat: 1, After: step})
	select {
	case f := <-fades:
		t.Fatalf("fired before any seat report: %+v", f)
	case <-time.After(3 * step):
	}
	h.send(ports.WaylandCapabilities{Capabilities: ports.Capabilities{Seats: []uint32{1}}})
	if f := <-fades; !f.Black {
		t.Fatal("deadline did not fire once the idle seat was reported")
	}
}

func TestRemovedSeatNoLongerBlocksDeadline(t *testing.T) {
	h := newHarness(t, cfgOf(0, step, 0, 0), nil)
	fades := make(chan ports.VisualFade, 4)
	h.visual.EXPECT().Fade(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, f ports.VisualFade) error { fades <- f; return nil })
	h.visual.EXPECT().Close().Return(nil).Once()
	h.deadlines()
	h.send(ports.WaylandCapabilities{Capabilities: ports.Capabilities{Seats: []uint32{1, 2}}})
	h.send(ports.WaylandIdle{Seat: 1, After: step})
	h.send(ports.WaylandCapabilities{Capabilities: ports.Capabilities{Seats: []uint32{1}}})
	if f := <-fades; !f.Black {
		t.Fatal("removing the busy seat did not release the deadline")
	}
}

func TestClosedOptionalChannelsAndNilReplyAreTolerated(t *testing.T) {
	h := newHarness(t, cfgOf(0, 0, 0, 0), nil)
	h.visual.EXPECT().Close().Return(nil).Once()
	close(h.fail)
	close(h.ctl)
	time.Sleep(30 * time.Millisecond)
	select {
	case err := <-h.done:
		t.Fatalf("owner died on closed optional channel: %v", err)
	default:
	}
	// A request with no Reply channel must not panic or apply anything.
	h2 := newHarness(t, cfgOf(0, 0, 0, 0), nil)
	h2.visual.EXPECT().Close().Return(nil).Once()
	h2.ctl <- ports.ControlRequest{Kind: ports.ControlLock}
	if st := h2.status(); st.Acquiring || st.Protected {
		t.Fatalf("nil-reply lock request applied: %+v", st)
	}
}

func TestSpawnOmitsStartupAndCurrentPINEnvNames(t *testing.T) {
	cfg := cfgOf(0, 0, 0, 0)
	cfg.Auth = ports.AuthConfig{Mode: ports.AuthModePIN, PINSource: ports.PINSourceEnv, PINEnv: "STARTUP_PIN"}
	h := newHarness(t, cfg, nil)
	locks := h.lockCapture()
	h.visual.EXPECT().Detach().Return(nil).Once()
	h.deadlines()
	next := cfg
	next.Auth = ports.AuthConfig{Mode: ports.AuthModePIN, PINSource: ports.PINSourceEnv, PINEnv: "OTHER_PIN"}
	h.cfgCh <- ports.ConfigChanged{Config: next}
	h.sysEv <- ports.SystemLockRequested{}
	l := lockOf(t, locks)
	if len(l.Spawn.OmitEnv) != 2 || l.Spawn.OmitEnv[0] != "OTHER_PIN" || l.Spawn.OmitEnv[1] != "STARTUP_PIN" {
		t.Fatalf("omit %v", l.Spawn.OmitEnv)
	}
	// Pass source after reload: the startup env name is still kept out.
	next.Auth = ports.AuthConfig{Mode: ports.AuthModePIN, PINSource: ports.PINSourcePass, PINEntry: "afk/pin"}
	h.cfgCh <- ports.ConfigChanged{Config: next}
	h.status()
}

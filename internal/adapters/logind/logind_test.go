package logind

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/mock"
	"golang.org/x/sys/unix"
)

const testOwner = ":1.42"
const testSession dbus.ObjectPath = "/org/freedesktop/login1/session/c1"

func expectProbe(t *testing.T, b *mockbus, uid uint32) {
	t.Helper()
	bounded := mock.MatchedBy(func(ctx context.Context) bool { d, ok := ctx.Deadline(); return ok && time.Until(d) <= callTimeout })
	b.EXPECT().call(bounded, "org.freedesktop.DBus", dbus.ObjectPath("/org/freedesktop/DBus"), "org.freedesktop.DBus.GetNameOwner", []any{service}).Return([]any{testOwner}, nil).Once()
	b.EXPECT().call(bounded, testOwner, managerPath, managerInterface+".GetSessionByPID", []any{uint32(os.Getpid())}).Return([]any{testSession}, nil).Once()
	b.EXPECT().call(bounded, testOwner, testSession, "org.freedesktop.DBus.Properties.Get", []any{sessionInterface, "User"}).Return([]any{dbus.MakeVariant([]any{uid, dbus.ObjectPath("/org/freedesktop/login1/user/_1000")})}, nil).Once()
}
func TestProbeCurrentPIDRealUIDAndReadOnly(t *testing.T) {
	b := newMockbus(t)
	expectProbe(t, b, uint32(os.Getuid()))
	b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".CanSuspend").Return([]any{"yes"}, nil).Once()
	b.EXPECT().call(mock.Anything, testOwner, managerPath, "org.freedesktop.DBus.Properties.Get", []any{managerInterface, "InhibitDelayMaxUSec"}).Return([]any{dbus.MakeVariant(uint64(5_000_000))}, nil).Once()
	b.EXPECT().unixFDs().Return(true).Once()
	a := newAdapter(b, ports.SystemRequirements{Session: true, Sleep: true})
	if err := a.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := a.Capabilities(); !c.Session || !c.Suspend || c.DelayMax != 5*time.Second {
		t.Fatalf("capabilities %+v", c)
	}
	if path, source := a.Session(); path != string(testSession) || source != "pid" {
		t.Fatalf("session %q source %q", path, source)
	}
	// No mutating call was configured: generated mock rejects Inhibit/Suspend.
}

const testUser dbus.ObjectPath = "/org/freedesktop/login1/user/_1000"
const staleSession dbus.ObjectPath = "/org/freedesktop/login1/session/c9"

var errPID, errXDG, errUser = errors.New("pid lookup"), errors.New("xdg lookup"), errors.New("user lookup")

// expectNoPIDSession answers the owner lookup and fails the PID session.
func expectNoPIDSession(b *mockbus) {
	b.EXPECT().call(mock.Anything, "org.freedesktop.DBus", dbus.ObjectPath("/org/freedesktop/DBus"), "org.freedesktop.DBus.GetNameOwner", []any{service}).Return([]any{testOwner}, nil).Once()
	b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".GetSessionByPID", []any{uint32(os.Getpid())}).Return(nil, errPID).Once()
}
func expectSessionProperty(b *mockbus, path dbus.ObjectPath, name string, value any) {
	b.EXPECT().call(mock.Anything, testOwner, path, "org.freedesktop.DBus.Properties.Get", []any{sessionInterface, name}).Return([]any{dbus.MakeVariant(value)}, nil).Once()
}

// expectSessionKind answers Type, then State only when Type is graphical.
func expectSessionKind(b *mockbus, path dbus.ObjectPath, typ, state string) {
	expectSessionProperty(b, path, "Type", typ)
	if typ == "wayland" || typ == "x11" {
		expectSessionProperty(b, path, "State", state)
	}
}
func expectSessionUID(b *mockbus, path dbus.ObjectPath, uid uint32) {
	expectSessionProperty(b, path, "User", []any{uid, testUser})
}

// expectDisplay answers the GetUser + User.Display fallback with display.
func expectDisplay(b *mockbus, display any) {
	b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".GetUser", []any{uint32(os.Getuid())}).Return([]any{testUser}, nil).Once()
	b.EXPECT().call(mock.Anything, testOwner, testUser, "org.freedesktop.DBus.Properties.Get", []any{userInterface, "Display"}).Return([]any{dbus.MakeVariant(display)}, nil).Once()
}

// A compositor run as a systemd user service gives its children no PID
// session; the display manager's live graphical XDG_SESSION_ID is used,
// UID-checked.
func TestProbeFallsBackToXDGSessionID(t *testing.T) {
	t.Setenv("XDG_SESSION_ID", "c1")
	for name, uid := range map[string]uint32{"same uid": uint32(os.Getuid()), "other uid": uint32(os.Getuid()) + 1} {
		t.Run(name, func(t *testing.T) {
			b := newMockbus(t)
			expectNoPIDSession(b)
			b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".GetSession", []any{"c1"}).Return([]any{testSession}, nil).Once()
			expectSessionKind(b, testSession, "wayland", "active")
			expectSessionUID(b, testSession, uid)
			a := newAdapter(b, ports.SystemRequirements{Session: true})
			err := a.Probe(context.Background())
			if ok := uid == uint32(os.Getuid()); ok != (err == nil) || ok != a.Capabilities().Session || ok != (a.session == testSession) {
				t.Fatalf("err=%v caps=%+v session=%q", err, a.Capabilities(), a.session)
			}
			want := ""
			if uid == uint32(os.Getuid()) {
				want = "XDG_SESSION_ID"
			}
			if _, source := a.Session(); source != want {
				t.Fatalf("source %q, want %q", source, want)
			}
		})
	}
}

// A terminal started by the compositor can inherit a stale XDG_SESSION_ID and
// run outside any session scope. An unknown, non-graphical or closing XDG
// session falls through to the real UID's live graphical Display session.
func TestProbeFallsBackToDisplaySession(t *testing.T) {
	cases := map[string]struct {
		xdg       string
		xdgExists bool
		typ       string
		state     string
	}{
		"no XDG_SESSION_ID":      {},
		"unknown XDG_SESSION_ID": {xdg: "c9"},
		"tty XDG_SESSION_ID":     {xdg: "c9", xdgExists: true, typ: "tty"},
		"closing XDG_SESSION_ID": {xdg: "c9", xdgExists: true, typ: "wayland", state: "closing"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_SESSION_ID", tc.xdg)
			b := newMockbus(t)
			expectNoPIDSession(b)
			switch {
			case tc.xdgExists:
				b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".GetSession", []any{tc.xdg}).Return([]any{staleSession}, nil).Once()
				expectSessionKind(b, staleSession, tc.typ, tc.state)
			case tc.xdg != "":
				b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".GetSession", []any{tc.xdg}).Return(nil, errXDG).Once()
			}
			expectDisplay(b, []any{"c1", testSession})
			expectSessionKind(b, testSession, "wayland", "active")
			expectSessionUID(b, testSession, uint32(os.Getuid()))
			a := newAdapter(b, ports.SystemRequirements{Session: true})
			if err := a.Probe(context.Background()); err != nil || a.session != testSession || !a.Capabilities().Session {
				t.Fatalf("err=%v session=%q", err, a.session)
			}
			if _, source := a.Session(); source != "user display" {
				t.Fatalf("source %q", source)
			}
		})
	}
}

func TestProbeRejectsDisplaySessionOfOtherUID(t *testing.T) {
	t.Setenv("XDG_SESSION_ID", "")
	b := newMockbus(t)
	expectNoPIDSession(b)
	expectDisplay(b, []any{"c1", testSession})
	expectSessionKind(b, testSession, "wayland", "active")
	expectSessionUID(b, testSession, uint32(os.Getuid())+1)
	a := newAdapter(b, ports.SystemRequirements{Session: true})
	if err := a.Probe(context.Background()); err == nil || a.Capabilities().Session {
		t.Fatal("other UID admitted through the Display fallback")
	}
}

// Every source's cause stays inspectable through the joined error.
func TestProbeWithoutAnySessionReportsEveryCause(t *testing.T) {
	t.Setenv("XDG_SESSION_ID", "c9")
	b := newMockbus(t)
	expectNoPIDSession(b)
	b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".GetSession", []any{"c9"}).Return(nil, errXDG).Once()
	b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".GetUser", []any{uint32(os.Getuid())}).Return(nil, errUser).Once()
	err := newAdapter(b, ports.SystemRequirements{Session: true}).Probe(context.Background())
	for _, cause := range []error{errPID, errXDG, errUser} {
		if !errors.Is(err, cause) {
			t.Fatalf("err=%v lacks %v", err, cause)
		}
	}
}

func TestDisplaySessionRejectsMalformedReplies(t *testing.T) {
	cases := map[string]struct {
		user    []any
		display any
	}{
		"empty user reply":   {user: []any{}},
		"non-path user":      {user: []any{"_1000"}},
		"display not tuple":  {user: []any{testUser}, display: "c1"},
		"display short":      {user: []any{testUser}, display: []any{"c1"}},
		"display not a path": {user: []any{testUser}, display: []any{"c1", "c1"}},
		"no display session": {user: []any{testUser}, display: []any{"", dbus.ObjectPath("/")}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := newMockbus(t)
			b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".GetUser", []any{uint32(os.Getuid())}).Return(tc.user, nil).Once()
			if tc.display != nil {
				b.EXPECT().call(mock.Anything, testOwner, testUser, "org.freedesktop.DBus.Properties.Get", []any{userInterface, "Display"}).Return([]any{dbus.MakeVariant(tc.display)}, nil).Once()
			}
			a := newAdapter(b, ports.SystemRequirements{Session: true})
			a.owner = testOwner
			if path, err := a.displaySession(context.Background()); err == nil {
				t.Fatalf("accepted %q", path)
			}
		})
	}
}

func TestProbeRejectsOtherUID(t *testing.T) {
	b := newMockbus(t)
	expectProbe(t, b, uint32(os.Getuid())+1)
	if err := newAdapter(b, ports.SystemRequirements{Session: true}).Probe(context.Background()); err == nil {
		t.Fatal("other UID admitted")
	}
}
func TestSignalFilteringAndUnlockNotAuthority(t *testing.T) {
	cases := []struct {
		sig   *dbus.Signal
		valid bool
	}{
		{&dbus.Signal{Sender: testOwner, Path: testSession, Name: sessionInterface + ".Lock"}, true},
		{&dbus.Signal{Sender: testOwner, Path: testSession, Name: sessionInterface + ".Unlock"}, true},
		{&dbus.Signal{Sender: testOwner, Path: managerPath, Name: managerInterface + ".PrepareForSleep", Body: []any{true}}, true},
		{&dbus.Signal{Sender: ":1.99", Path: testSession, Name: sessionInterface + ".Lock"}, false},
		{&dbus.Signal{Sender: testOwner, Path: "/org/freedesktop/login1/session/other", Name: sessionInterface + ".Unlock"}, false},
		{&dbus.Signal{Sender: testOwner, Path: managerPath, Name: managerInterface + ".PrepareForSleep", Body: []any{"true"}}, false},
		{&dbus.Signal{Sender: testOwner, Path: testSession, Name: sessionInterface + ".Lock", Body: []any{true}}, false},
	}
	for _, tc := range cases {
		event, ok := parseSignal(tc.sig, testOwner, testSession)
		if ok != tc.valid {
			t.Fatalf("signal %+v accepted=%v", tc.sig, ok)
		}
		if tc.sig.Name == sessionInterface+".Unlock" && ok {
			if _, ok := event.(ports.SystemUnlockObserved); !ok {
				t.Fatalf("unlock converted to authority %T", event)
			}
		}
	}
}
func TestInhibitorOwnsCloexecDuplicateAndClosesOnce(t *testing.T) {
	b := newMockbus(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	received, err := unix.Dup(int(r.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".Inhibit", []any{"sleep", "neferafk", "session protection", "delay"}).Return([]any{dbus.UnixFD(received)}, nil).Once()
	b.EXPECT().close().Return(nil).Once()
	a := newAdapter(b, ports.SystemRequirements{Sleep: true})
	a.owner = testOwner
	if err := a.inhibit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(received), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("received FD not closed: %v", err)
	}
	held := a.inhibitor
	flags, err := unix.FcntlInt(held.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("inhibitor not CLOEXEC")
	}
	a.release()
	a.release()
	if _, err := held.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("owned inhibitor still open")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestRunMatchingSleepCycleAndSuspendFalse(t *testing.T) {
	b := newMockbus(t)
	a := newAdapter(b, ports.SystemRequirements{Session: true, Sleep: true})
	a.owner = testOwner
	a.session = testSession
	a.caps = ports.SystemCapabilities{Session: true, Suspend: true, DelayMax: 5 * time.Second}
	b.EXPECT().signals(mock.Anything).Return().Once()
	b.EXPECT().removeSignals(mock.Anything).Return().Once()
	b.EXPECT().match(mock.Anything, mock.Anything).Return(nil).Twice()
	b.EXPECT().unixFDs().Return(true).Once()
	b.EXPECT().close().Return(nil).Once()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	received, err := unix.Dup(int(r.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".Inhibit", []any{"sleep", "neferafk", "session protection", "delay"}).Return([]any{dbus.UnixFD(received)}, nil).Once()
	suspended := make(chan struct{})
	b.EXPECT().call(mock.Anything, testOwner, managerPath, "org.freedesktop.DBus.Properties.Get", []any{managerInterface, "BlockInhibited"}).Return([]any{dbus.MakeVariant("")}, nil).Once()
	b.EXPECT().suspend(mock.Anything, testOwner).RunAndReturn(func(ctx context.Context, _ string) <-chan *dbus.Call {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded suspend call")
		}
		close(suspended)
		return make(chan *dbus.Call, 1) // reply held: owner must still process SleepReady
	}).Once()
	commands := make(chan ports.SystemCommand)
	events := make(chan ports.SystemEvent, 8)
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background(), commands, events) }()
	<-events // capability report synchronizes initial inhibitor ownership
	a.signals <- &dbus.Signal{Sender: testOwner, Path: managerPath, Name: managerInterface + ".PrepareForSleep", Body: []any{true}}
	prep := (<-events).(ports.SleepPreparation)
	if prep.Cycle != 1 || prep.MaxDelay != 5*time.Second || prep.ReceivedAt.IsZero() {
		t.Fatalf("sleep prep %+v", prep)
	}
	commands <- ports.SystemSleepReady{Cycle: 99}
	commands <- ports.SystemSuspend{}
	<-suspended // ordered owner checkpoint, no live suspend involved
	held := a.inhibitor
	if held == nil {
		t.Fatal("stale cycle released inhibitor")
	}
	commands <- ports.SystemSleepReady{Cycle: prep.Cycle}
	close(commands)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run blocked")
	}
	if _, err := held.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("matching ready did not close inhibitor")
	}
}
func TestOpenOffDoesNotConnect(t *testing.T) {
	if _, err := Open(context.Background(), ports.SystemRequirements{}); err == nil {
		t.Fatal("disabled adapter unexpectedly opened")
	}
}

func TestSuspendBlockedAndUnavailableNeverCallsSuspend(t *testing.T) {
	for _, blocked := range []bool{true, false} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			b := newMockbus(t)
			a := newAdapter(b, ports.SystemRequirements{Sleep: true})
			a.owner = testOwner
			a.caps.Suspend = blocked
			b.EXPECT().signals(mock.Anything).Return().Once()
			b.EXPECT().removeSignals(mock.Anything).Return().Once()
			b.EXPECT().match(mock.Anything, mock.Anything).Return(nil).Once()
			b.EXPECT().unixFDs().Return(false).Once()
			b.EXPECT().close().Return(nil).Once()
			if blocked {
				b.EXPECT().call(mock.Anything, testOwner, managerPath, "org.freedesktop.DBus.Properties.Get", []any{managerInterface, "BlockInhibited"}).Return([]any{dbus.MakeVariant("sleep:shutdown")}, nil).Once()
			}
			commands := make(chan ports.SystemCommand, 1)
			events := make(chan ports.SystemEvent, 3)
			commands <- ports.SystemSuspend{}
			close(commands)
			if err := a.Run(context.Background(), commands, events); err != nil {
				t.Fatal(err)
			}
			<-events
			if failure, ok := (<-events).(ports.SystemFailure); !ok || failure.Operation != "suspend" {
				t.Fatal("missing unsupported/block failure")
			}
		})
	}
}

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
	// No mutating call was configured: generated mock rejects Inhibit/Suspend.
}

// A compositor run as a systemd user service gives its children no PID
// session; the display manager's XDG_SESSION_ID is used, UID-checked.
func TestProbeFallsBackToXDGSessionID(t *testing.T) {
	t.Setenv("XDG_SESSION_ID", "c1")
	for name, uid := range map[string]uint32{"same uid": uint32(os.Getuid()), "other uid": uint32(os.Getuid()) + 1} {
		t.Run(name, func(t *testing.T) {
			b := newMockbus(t)
			b.EXPECT().call(mock.Anything, "org.freedesktop.DBus", dbus.ObjectPath("/org/freedesktop/DBus"), "org.freedesktop.DBus.GetNameOwner", []any{service}).Return([]any{testOwner}, nil).Once()
			b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".GetSessionByPID", []any{uint32(os.Getpid())}).Return(nil, errors.New("no session")).Once()
			b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".GetSession", []any{"c1"}).Return([]any{testSession}, nil).Once()
			b.EXPECT().call(mock.Anything, testOwner, testSession, "org.freedesktop.DBus.Properties.Get", []any{sessionInterface, "User"}).Return([]any{dbus.MakeVariant([]any{uid, dbus.ObjectPath("/org/freedesktop/login1/user/_1000")})}, nil).Once()
			a := newAdapter(b, ports.SystemRequirements{Session: true})
			err := a.Probe(context.Background())
			if ok := uid == uint32(os.Getuid()); ok != (err == nil) || ok != a.Capabilities().Session || ok != (a.session == testSession) {
				t.Fatalf("err=%v caps=%+v session=%q", err, a.Capabilities(), a.session)
			}
		})
	}
}

func TestProbeWithoutPIDSessionOrXDGSessionIDFails(t *testing.T) {
	t.Setenv("XDG_SESSION_ID", "")
	b := newMockbus(t)
	b.EXPECT().call(mock.Anything, "org.freedesktop.DBus", dbus.ObjectPath("/org/freedesktop/DBus"), "org.freedesktop.DBus.GetNameOwner", []any{service}).Return([]any{testOwner}, nil).Once()
	b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".GetSessionByPID", []any{uint32(os.Getpid())}).Return(nil, errors.New("no session")).Once()
	if err := newAdapter(b, ports.SystemRequirements{Session: true}).Probe(context.Background()); err == nil {
		t.Fatal("probe succeeded without any session")
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

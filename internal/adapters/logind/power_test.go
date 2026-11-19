package logind

import (
	"context"
	"errors"
	"testing"
	"time"

	"bnema.dev/neferafk/internal/ports"
	"github.com/stretchr/testify/mock"
)

func TestProbePowerCapabilitiesNeverMissing(t *testing.T) {
	b := newMockbus(t)
	b.EXPECT().call(mock.Anything, "org.freedesktop.DBus", objectPath("/org/freedesktop/DBus"), "org.freedesktop.DBus.GetNameOwner", []any{service}).Return([]any{testOwner}, nil).Once()
	b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".CanReboot").Return([]any{"yes"}, nil).Once()
	b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".CanPowerOff").Return([]any{"challenge"}, nil).Once()
	a := newAdapter(b, ports.SystemRequirements{Power: true})
	if err := a.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := a.Capabilities()
	if !c.Reboot || c.RebootAuthorization != "yes" || c.PowerOff || c.PowerOffAuthorization != "challenge" || len(c.Missing) != 0 {
		t.Fatalf("capabilities %+v", c)
	}
}

func TestProbePowerErrorsDegradeOnly(t *testing.T) {
	b := newMockbus(t)
	b.EXPECT().call(mock.Anything, "org.freedesktop.DBus", objectPath("/org/freedesktop/DBus"), "org.freedesktop.DBus.GetNameOwner", []any{service}).Return([]any{testOwner}, nil).Once()
	b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".CanReboot").Return(nil, errors.New("denied")).Once()
	b.EXPECT().call(mock.Anything, testOwner, managerPath, managerInterface+".CanPowerOff").Return([]any{uint32(1)}, nil).Once()
	a := newAdapter(b, ports.SystemRequirements{Power: true})
	if err := a.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := a.Capabilities(); c.Reboot || c.PowerOff || c.RebootAuthorization != "error" || c.PowerOffAuthorization != "error" || len(c.Missing) != 0 {
		t.Fatalf("capabilities %+v", c)
	}
}

// runPower starts Run with the given commands already queued and returns the
// events channel and Run's result channel.
func runPower(t *testing.T, a *Adapter, b *mockbus, cmds ...ports.SystemCommand) (chan ports.SystemEvent, chan ports.SystemCommand, chan error) {
	t.Helper()
	b.EXPECT().signals(mock.Anything).Once()
	b.EXPECT().removeSignals(mock.Anything).Once()
	b.EXPECT().match(mock.Anything, mock.Anything).Return(nil).Maybe() // only with Session or Sleep
	b.EXPECT().close().Return(nil).Once()
	commands := make(chan ports.SystemCommand, len(cmds))
	for _, c := range cmds {
		commands <- c
	}
	events := make(chan ports.SystemEvent, 8)
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background(), commands, events) }()
	if _, ok := (<-events).(ports.SystemCapabilityReport); !ok {
		t.Fatal("missing capability report")
	}
	return events, commands, done
}

func failure(t *testing.T, events chan ports.SystemEvent) ports.SystemFailure {
	t.Helper()
	select {
	case e := <-events:
		f, ok := e.(ports.SystemFailure)
		if !ok {
			t.Fatalf("unexpected event %#v", e)
		}
		return f
	case <-time.After(time.Second):
		t.Fatal("missing failure")
	}
	return ports.SystemFailure{}
}

func TestRebootAndPowerOffCallLogindBoundedNonInteractive(t *testing.T) {
	for _, tc := range []struct {
		cmd        ports.SystemCommand
		op, method string
	}{{ports.SystemReboot{}, "reboot", "Reboot"}, {ports.SystemPowerOff{}, "poweroff", "PowerOff"}} {
		t.Run(tc.op, func(t *testing.T) {
			b := newMockbus(t)
			a := newAdapter(b, ports.SystemRequirements{Power: true})
			a.owner = testOwner
			a.caps.Reboot, a.caps.PowerOff = true, true
			b.EXPECT().call(mock.Anything, testOwner, managerPath, "org.freedesktop.DBus.Properties.Get", []any{managerInterface, "BlockInhibited"}).Return([]any{makeVariant("sleep:idle")}, nil).Once()
			reply := make(chan *actionResult, 1)
			reply <- &actionResult{Err: errors.New("denied")}
			b.EXPECT().powerAction(mock.Anything, testOwner, tc.method).RunAndReturn(func(ctx context.Context, _, _ string) <-chan *actionResult {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("unbounded power call")
				}
				return reply
			}).Once()
			events, commands, done := runPower(t, a, b, tc.cmd)
			if f := failure(t, events); f.Operation != tc.op || f.Err == nil {
				t.Fatalf("%#v", f)
			}
			close(commands)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPowerActionsRefusedWithoutCallingLogind(t *testing.T) {
	for name, tc := range map[string]struct {
		req     ports.SystemRequirements
		caps    bool
		blocked string
		cmd     ports.SystemCommand
		wantOp  string
	}{
		"reboot not required":     {ports.SystemRequirements{Sleep: true}, true, "", ports.SystemReboot{}, "reboot"},
		"poweroff unauthorized":   {ports.SystemRequirements{Power: true}, false, "", ports.SystemPowerOff{}, "poweroff"},
		"reboot shutdown block":   {ports.SystemRequirements{Power: true}, true, "sleep:shutdown", ports.SystemReboot{}, "reboot"},
		"poweroff shutdown block": {ports.SystemRequirements{Power: true}, true, "shutdown", ports.SystemPowerOff{}, "poweroff"},
		"suspend without sleep":   {ports.SystemRequirements{Power: true}, true, "", ports.SystemSuspend{}, "suspend"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newMockbus(t)
			a := newAdapter(b, tc.req)
			a.owner = testOwner
			a.caps.Reboot, a.caps.PowerOff, a.caps.Suspend = tc.caps, tc.caps, tc.caps
			if tc.req.Sleep {
				b.EXPECT().unixFDs().Return(false).Once()
			}
			if tc.blocked != "" {
				b.EXPECT().call(mock.Anything, testOwner, managerPath, "org.freedesktop.DBus.Properties.Get", []any{managerInterface, "BlockInhibited"}).Return([]any{makeVariant(tc.blocked)}, nil).Once()
			}
			events, commands, done := runPower(t, a, b, tc.cmd)
			if f := failure(t, events); f.Operation != tc.wantOp {
				t.Fatalf("%#v", f)
			}
			close(commands)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOnePowerRequestPendingAtATime(t *testing.T) {
	b := newMockbus(t)
	a := newAdapter(b, ports.SystemRequirements{Power: true})
	a.owner = testOwner
	a.caps.Reboot, a.caps.PowerOff = true, true
	b.EXPECT().call(mock.Anything, testOwner, managerPath, "org.freedesktop.DBus.Properties.Get", []any{managerInterface, "BlockInhibited"}).Return([]any{makeVariant("")}, nil).Once()
	b.EXPECT().powerAction(mock.Anything, testOwner, "Reboot").Return(make(chan *actionResult)).Once()
	events, commands, done := runPower(t, a, b, ports.SystemReboot{}, ports.SystemPowerOff{})
	if f := failure(t, events); f.Operation != "poweroff" {
		t.Fatalf("%#v", f)
	}
	close(commands)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

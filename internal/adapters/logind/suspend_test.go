package logind

import (
	"context"
	"errors"
	"testing"
	"time"

	"bnema.dev/neferafk/internal/ports"
	"github.com/stretchr/testify/mock"
)

func TestRunSuspendFailureAndCancellation(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "reply failure", true: "cancel pending"}[cancelRun], func(t *testing.T) {
			b := newMockbus(t)
			a := newAdapter(b, ports.SystemRequirements{Sleep: true})
			a.owner = testOwner
			a.caps.Suspend = true
			b.EXPECT().signals(mock.Anything).Once()
			b.EXPECT().removeSignals(mock.Anything).Once()
			b.EXPECT().match(mock.Anything, signalMatch{Sender: testOwner, Path: managerPath, Interface: managerInterface}).Return(nil).Once()
			b.EXPECT().unixFDs().Return(false).Once()
			b.EXPECT().close().Return(nil).Once()
			b.EXPECT().call(mock.Anything, testOwner, managerPath, "org.freedesktop.DBus.Properties.Get", []any{managerInterface, "BlockInhibited"}).Return([]any{makeVariant("")}, nil).Once()
			started := make(chan context.Context, 1)
			replies := make(chan *suspendResult, 1)
			failure := errors.New("suspend denied")
			if !cancelRun {
				replies <- &suspendResult{Err: failure}
			}
			b.EXPECT().suspend(mock.Anything, testOwner).RunAndReturn(func(ctx context.Context, _ string) <-chan *suspendResult { started <- ctx; return replies }).Once()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			commands := make(chan ports.SystemCommand, 1)
			events := make(chan ports.SystemEvent, 3)
			done := make(chan error, 1)
			commands <- ports.SystemSuspend{}
			go func() { done <- a.Run(ctx, commands, events) }()
			var pending context.Context
			select {
			case pending = <-started:
			case <-time.After(time.Second):
				t.Fatal("suspend not started")
			}
			if cancelRun {
				cancel()
			} else {
				<-events
				select {
				case event := <-events:
					f, ok := event.(ports.SystemFailure)
					if !ok || f.Operation != "suspend" || !errors.Is(f.Err, failure) {
						t.Fatalf("unexpected failure: %#v", event)
					}
				case <-time.After(time.Second):
					t.Fatal("missing suspend failure")
				}
				close(commands)
			}
			select {
			case err := <-done:
				if cancelRun && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Run blocked")
			}
			select {
			case <-pending.Done():
			default:
				t.Fatal("pending suspend context not canceled")
			}
		})
	}
}

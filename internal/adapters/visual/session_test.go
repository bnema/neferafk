package visual

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/bnema/neferafk/internal/adapters/visualproc"
	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/nefergui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type harness struct {
	cmdW   *io.PipeWriter
	events *io.PipeReader
	done   chan error
	cfgs   chan nefergui.LockConfig
	l      *mockworkerLauncher
	lock   *mocklockRunner
	fade   *mockfadeRunner
}

// newHarness runs a session over real pipes; RunLock is a mock that publishes
// its config and returns nil only once Unlock fires (or ctx ends).
func newHarness(t *testing.T) *harness { return newHarnessRun(t, nil) }

// newHarnessRun is newHarness with an optional replacement RunLock body.
func newHarnessRun(t *testing.T, runLock func(context.Context, nefergui.LockConfig) error) *harness {
	t.Helper()
	cmdR, cmdW := io.Pipe()
	evR, evW := io.Pipe()
	h := &harness{cmdW: cmdW, events: evR, done: make(chan error, 1), cfgs: make(chan nefergui.LockConfig, 1),
		l: newMockworkerLauncher(t), lock: newMocklockRunner(t), fade: newMockfadeRunner(t)}
	if runLock == nil {
		runLock = func(ctx context.Context, cfg nefergui.LockConfig) error {
			h.cfgs <- cfg
			select {
			case <-cfg.Unlock:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	h.lock.EXPECT().RunLock(mock.Anything, mock.Anything).RunAndReturn(runLock).Maybe()
	s := &session{log: testLog(), cmds: cmdR, events: evW, locker: h.lock, fader: h.fade, launcher: h.l}
	go func() { h.done <- s.run(context.Background()) }()
	t.Cleanup(func() { cmdW.Close(); evR.Close() })
	return h
}

func (h *harness) sendLock(t *testing.T) {
	require.NoError(t, visualproc.WriteCommand(h.cmdW, ports.VisualLock{Generation: gen, Auth: boot()}))
}

func (h *harness) event(t *testing.T) ports.VisualEvent {
	t.Helper()
	ch := make(chan ports.VisualEvent, 1)
	go func() { ev, _ := visualproc.ReadEvent(h.events); ch <- ev }()
	return recv(t, ch)
}

func TestLockReleasedOnlyAfterAuthSuccess(t *testing.T) {
	h := newHarness(t)
	w := expectWorker(t, h.l)
	h.sendLock(t)
	cfg := recv(t, h.cfgs)
	w.bootstrap(t)
	w.ready(t)
	cfg.OnLocked()
	require.Equal(t, ports.LockConfirmed{Generation: gen}, h.event(t))

	cfg.OnSubmit([]byte("bad"))
	f := w.attempt(t)
	w.result(t, f.Attempt, ports.AuthDenied)
	require.Equal(t, nefergui.LockFailed, recvFailed(t, cfg.Status))
	select {
	case err := <-h.done:
		t.Fatalf("session ended after denial: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cfg.OnSubmit([]byte("good"))
	f = w.attempt(t)
	w.result(t, f.Attempt, ports.AuthSuccess)
	require.Equal(t, ports.LockReleased{Generation: gen}, h.event(t))
	require.NoError(t, recv(t, h.done))
	recv(t, w.killed) // worker reaped with the lock
}

func recvFailed(t *testing.T, c <-chan nefergui.LockStatus) nefergui.LockStatus {
	for {
		if s := recv(t, c); s == nefergui.LockFailed {
			return s
		}
	}
}

func TestWorkerCrashKeepsLock(t *testing.T) {
	h := newHarness(t)
	w := expectWorker(t, h.l)
	h.sendLock(t)
	cfg := recv(t, h.cfgs)
	w.bootstrap(t)
	w.ready(t)
	cfg.OnLocked()
	h.event(t)
	cfg.OnSubmit([]byte("x"))
	w.attempt(t)
	w.toVisual.Close()
	recv(t, w.killed)
	recvFailed(t, cfg.Status)
	select {
	case err := <-h.done:
		t.Fatalf("worker crash ended the lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDaemonEOFKeepsLockUntilUnlock(t *testing.T) {
	h := newHarness(t)
	w := expectWorker(t, h.l)
	h.sendLock(t)
	cfg := recv(t, h.cfgs)
	w.bootstrap(t)
	w.ready(t)
	cfg.OnLocked()
	h.event(t)
	h.cmdW.Close() // daemon gone
	select {
	case err := <-h.done:
		t.Fatalf("EOF dropped an active lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cfg.OnSubmit([]byte("good"))
	f := w.attempt(t)
	w.result(t, f.Attempt, ports.AuthSuccess)
	require.Equal(t, ports.LockReleased{Generation: gen}, h.event(t))
	require.NoError(t, recv(t, h.done))
}

func TestFadeOnlyExitsOnEOF(t *testing.T) {
	h := newHarness(t)
	started := make(chan *fadeModel, 1)
	h.fade.EXPECT().RunFade(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, m *fadeModel) error {
		started <- m
		<-ctx.Done()
		return nil
	}).Once()
	require.NoError(t, visualproc.WriteCommand(h.cmdW, ports.VisualFade{Black: true, Duration: time.Second}))
	m := recv(t, started)
	require.Equal(t, fadeCmd{black: true, dur: time.Second}, recv(t, m.cmds))
	h.cmdW.Close()
	require.NoError(t, recv(t, h.done))
}

func TestRevealWithoutFadeExitsWithoutSpawning(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, visualproc.WriteCommand(h.cmdW, ports.VisualFade{Black: false}))
	require.NoError(t, recv(t, h.done))
}

func TestFinishedRevealExits(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.fade.EXPECT().RunFade(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, m *fadeModel) error {
		<-release
		return nil
	}).Once()
	require.NoError(t, visualproc.WriteCommand(h.cmdW, ports.VisualFade{Black: true}))
	close(release)
	require.NoError(t, recv(t, h.done))
}

// noEvent fails if the session emitted anything on the event pipe.
func (h *harness) noEvent(t *testing.T) {
	t.Helper()
	got := make(chan ports.VisualEvent, 1)
	go func() {
		ev, err := visualproc.ReadEvent(h.events)
		if err == nil {
			got <- ev
		}
	}()
	select {
	case ev := <-got:
		t.Fatalf("unexpected event %#v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRunLockNilWithoutAuthSuccessNeverReleases(t *testing.T) {
	h := newHarnessRun(t, func(context.Context, nefergui.LockConfig) error { return nil })
	w := expectWorker(t, h.l)
	h.sendLock(t)
	w.bootstrap(t)
	err := recv(t, h.done)
	require.ErrorIs(t, err, errLockWithoutAuth)
	h.noEvent(t) // no LockReleased
}

func TestRunLockErrorNeverReleases(t *testing.T) {
	for name, runErr := range map[string]error{
		"finished": nefergui.ErrLockFinished,
		"wrapped":  fmt.Errorf("compositor: %w", nefergui.ErrLockFinished),
		"other":    errors.New("boom"),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarnessRun(t, func(context.Context, nefergui.LockConfig) error { return runErr })
			w := expectWorker(t, h.l)
			h.sendLock(t)
			w.bootstrap(t)
			require.ErrorIs(t, recv(t, h.done), runErr)
			h.noEvent(t)
		})
	}
}

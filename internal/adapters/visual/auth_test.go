package visual

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/bnema/neferafk/internal/adapters/auth"
	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/zerowrap"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const gen = 7

func testLog() zerowrap.Logger { return zerowrap.New(zerowrap.Config{Output: io.Discard}) }

// testWorker is the test's end of one worker's private pipes.
type testWorker struct {
	fromVisual *io.PipeReader // bootstrap and frames the visual wrote
	toVisual   *io.PipeWriter // frames the worker answers with
	killed     chan struct{}
}

// expectWorker queues one launch on l, backed by real in-memory pipes.
func expectWorker(t *testing.T, l *mockworkerLauncher) *testWorker {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	w := &testWorker{fromVisual: inR, toVisual: outW, killed: make(chan struct{})}
	proc := newMockworkerProcess(t)
	proc.EXPECT().pipes().Return(inW, outR).Maybe()
	proc.EXPECT().kill().Run(func() { close(w.killed) }).Return(nil).Maybe()
	proc.EXPECT().wait().Return(nil).Maybe()
	l.EXPECT().launch(mock.Anything).Return(proc, nil).Once()
	t.Cleanup(func() { inR.Close(); outW.Close() })
	return w
}

func (w *testWorker) bootstrap(t *testing.T) ports.AuthBootstrap {
	t.Helper()
	b, err := auth.ReadBootstrap(w.fromVisual)
	require.NoError(t, err)
	return b
}

func (w *testWorker) send(t *testing.T, f ports.AuthFrame) {
	t.Helper()
	f.Generation = gen
	require.NoError(t, auth.WriteFrame(w.toVisual, f))
}

func (w *testWorker) ready(t *testing.T) {
	w.send(t, ports.AuthFrame{Kind: ports.AuthReady, Code: ports.AuthPassword})
}

func (w *testWorker) result(t *testing.T, attempt uint64, c ports.AuthCode) {
	w.send(t, ports.AuthFrame{Kind: ports.AuthResult, Code: c, Attempt: attempt})
}

func (w *testWorker) attempt(t *testing.T) ports.AuthFrame {
	t.Helper()
	f, err := auth.ReadFrame(w.fromVisual)
	require.NoError(t, err)
	return f
}

func recv[T any](t *testing.T, c <-chan T) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	panic("unreachable")
}

func boot() ports.AuthBootstrap {
	return ports.AuthBootstrap{Generation: gen, PINSource: "env", EnvPIN: []byte("pin")}
}

func startCtl(t *testing.T, l workerLauncher, timeout time.Duration) *authController {
	c := newAuthController(testLog(), l, boot(), timeout)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); c.run(ctx) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
	return c
}

func TestSuccessRequestsUnlockOnlyAfterAuthSuccess(t *testing.T) {
	l := newMockworkerLauncher(t)
	w := expectWorker(t, l)
	c := startCtl(t, l, time.Minute)
	b := w.bootstrap(t)
	require.Equal(t, uint64(gen), b.Generation)
	w.ready(t)
	c.submit([]byte("hunter2"))
	f := w.attempt(t)
	require.Equal(t, ports.AuthAttempt, f.Kind)
	require.Equal(t, []byte("hunter2"), f.Payload)
	select {
	case <-c.unlock:
		t.Fatal("unlock requested before the result")
	case <-time.After(50 * time.Millisecond):
	}
	w.result(t, f.Attempt, ports.AuthSuccess)
	recv(t, c.unlock)
	require.True(t, c.succeeded.Load())
}

func TestDeniedStaysLockedAndAllowsRetry(t *testing.T) {
	l := newMockworkerLauncher(t)
	w := expectWorker(t, l)
	c := startCtl(t, l, time.Minute)
	w.bootstrap(t)
	w.ready(t)
	c.submit([]byte("bad"))
	f := w.attempt(t)
	w.result(t, f.Attempt, ports.AuthDenied)
	for recv(t, c.status) != lockFailed {
	}
	select {
	case <-c.unlock:
		t.Fatal("denied must not unlock")
	case <-time.After(50 * time.Millisecond):
	}
	c.submit([]byte("good"))
	f = w.attempt(t)
	require.Equal(t, []byte("good"), f.Payload)
	w.result(t, f.Attempt, ports.AuthSuccess)
	recv(t, c.unlock)
}

func TestWorkerCrashStaysLockedAndRespawnsOnNextAttempt(t *testing.T) {
	l := newMockworkerLauncher(t)
	w1 := expectWorker(t, l)
	c := startCtl(t, l, time.Minute)
	w1.bootstrap(t)
	w1.ready(t)
	c.submit([]byte("x"))
	w1.attempt(t)
	w1.toVisual.Close() // worker died mid-attempt
	recv(t, w1.killed)
	for recv(t, c.status) != lockFailed {
	}
	require.False(t, c.succeeded.Load())
	w2 := expectWorker(t, l)
	c.submit([]byte("again"))
	w2.bootstrap(t)
	w2.ready(t)
	f := w2.attempt(t)
	require.Equal(t, []byte("again"), f.Payload)
	w2.result(t, f.Attempt, ports.AuthSuccess)
	recv(t, c.unlock)
}

func TestWatchdogKillsHungWorkerAndStaysLocked(t *testing.T) {
	l := newMockworkerLauncher(t)
	w := expectWorker(t, l)
	c := startCtl(t, l, 100*time.Millisecond)
	w.bootstrap(t)
	w.ready(t)
	c.submit([]byte("x"))
	w.attempt(t) // no result ever comes
	recv(t, w.killed)
	for recv(t, c.status) != lockFailed {
	}
	select {
	case <-c.unlock:
		t.Fatal("timeout must not unlock")
	default:
	}
}

func TestSubmittedCopiesAreWiped(t *testing.T) {
	l := newMockworkerLauncher(t)
	w := expectWorker(t, l)
	c := newAuthController(testLog(), l, boot(), time.Minute)
	ctx := context.Background()
	spawned := make(chan struct{})
	go func() { c.spawn(ctx); close(spawned) }() // owner role: nothing else runs
	w.bootstrap(t)
	recv(t, spawned)
	t.Cleanup(c.drop)
	c.onFrame(frameMsg{f: ports.AuthFrame{Kind: ports.AuthReady, Code: ports.AuthPassword, Generation: gen}})

	orig := []byte("hunter2")
	c.submit(orig)
	cp := <-c.submits
	require.NotSame(t, &orig[0], &cp[0], "submit hands the controller a private copy")
	got := make(chan []byte, 1)
	go func() { got <- w.attempt(t).Payload }()
	c.onSubmit(ctx, cp)
	require.Equal(t, []byte("hunter2"), recv(t, got))
	require.Equal(t, make([]byte, len("hunter2")), cp, "copy wiped once written to the worker pipe")
}

func TestPromptAndFallbackAreReflected(t *testing.T) {
	l := newMockworkerLauncher(t)
	w := expectWorker(t, l)
	c := startCtl(t, l, time.Minute)
	w.bootstrap(t)
	w.send(t, ports.AuthFrame{Kind: ports.AuthReady, Code: ports.AuthFallbackPassword})
	recv(t, c.wake)
	require.Equal(t, promptFallback, promptKind(c.prompt.Load()))
	c.submit([]byte("x"))
	f := w.attempt(t)
	w.send(t, ports.AuthFrame{Kind: ports.AuthPrompt, Code: ports.AuthPassword, Attempt: f.Attempt})
	recv(t, c.wake)
	require.Equal(t, promptMore, promptKind(c.prompt.Load()))
}

func TestDropDrainsAndWipesQueuedSubmissions(t *testing.T) {
	l := newMockworkerLauncher(t)
	c := newAuthController(testLog(), l, boot(), time.Minute)
	queued := []byte("hunter2")
	c.submits <- queued
	c.drop()
	require.Equal(t, make([]byte, len("hunter2")), queued, "queued secret wiped")
	require.Empty(t, c.submits)

	// A submission racing the controller's exit is wiped by a later drain.
	late := []byte("late")
	c.submits <- late
	c.drainSubmits()
	require.Equal(t, make([]byte, len("late")), late)
}

func TestRunExitDrainsQueuedSubmission(t *testing.T) {
	l := newMockworkerLauncher(t)
	w := expectWorker(t, l)
	c := newAuthController(testLog(), l, boot(), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.run(ctx); close(done) }()
	w.bootstrap(t)
	cancel()
	recv(t, done)
	late := []byte("late-secret")
	c.submits <- late // UI handed a secret over after the controller left
	c.drainSubmits()
	require.Equal(t, make([]byte, len("late-secret")), late)
}

// A frame for another generation or attempt must never unlock.
func TestControllerKillsWorkerOnStaleGenerationOrAttempt(t *testing.T) {
	cases := map[string]func(attempt uint64) ports.AuthFrame{
		"wrong generation": func(a uint64) ports.AuthFrame {
			return ports.AuthFrame{Kind: ports.AuthResult, Code: ports.AuthSuccess, Generation: gen + 1, Attempt: a}
		},
		"old attempt": func(a uint64) ports.AuthFrame {
			return ports.AuthFrame{Kind: ports.AuthResult, Code: ports.AuthSuccess, Generation: gen, Attempt: a - 1}
		},
		"future attempt": func(a uint64) ports.AuthFrame {
			return ports.AuthFrame{Kind: ports.AuthResult, Code: ports.AuthSuccess, Generation: gen, Attempt: a + 1}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			l := newMockworkerLauncher(t)
			w := expectWorker(t, l)
			c := startCtl(t, l, time.Minute)
			w.bootstrap(t)
			w.ready(t)
			c.submit([]byte("first"))
			w.result(t, w.attempt(t).Attempt, ports.AuthDenied)
			for recv(t, c.status) != lockFailed {
			}
			c.submit([]byte("second")) // attempt 2: attempt 1 is now stale
			f := w.attempt(t)
			require.EqualValues(t, 2, f.Attempt)
			require.NoError(t, auth.WriteFrame(w.toVisual, mk(f.Attempt)))
			recv(t, w.killed)
			for recv(t, c.status) != lockFailed {
			}
			select {
			case <-c.unlock:
				t.Fatal("stale frame unlocked")
			case <-time.After(50 * time.Millisecond):
			}
			require.False(t, c.succeeded.Load())
		})
	}
}

// The watchdog must outlast the longest wrong-PIN delay, or killing the worker
// mid-delay would restart its schedule and defeat the rate limit.
func TestWatchdogOutlastsDenyDelay(t *testing.T) {
	require.GreaterOrEqual(t, workerTimeout, 30*time.Second+auth.MaxDenyDelay)
}

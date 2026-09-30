//go:build linux

package auth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	"golang.org/x/sys/unix"
)

const hardenChildEnv = "NEFERAFK_TEST_HARDEN_CHILD"

// TestHardenProcessDisablesDumpsAndCoreLimit runs in a subprocess because the
// helper changes process-wide state.
func TestHardenProcessDisablesDumpsAndCoreLimit(t *testing.T) {
	if os.Getenv(hardenChildEnv) == "1" {
		if dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0); err != nil || dumpable != 1 {
			t.Fatalf("precondition: dumpable=%d err=%v", dumpable, err)
		}
		if err := HardenProcess(); err != nil {
			t.Fatal(err)
		}
		if dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0); err != nil || dumpable != 0 {
			t.Fatalf("dumpable=%d err=%v", dumpable, err)
		}
		var lim unix.Rlimit
		if err := unix.Getrlimit(unix.RLIMIT_CORE, &lim); err != nil || lim.Cur != 0 || lim.Max != 0 {
			t.Fatalf("RLIMIT_CORE %+v err=%v", lim, err)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHardenProcessDisablesDumpsAndCoreLimit$")
	cmd.Env = append(os.Environ(), hardenChildEnv+"=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

func TestDenyDelaySchedule(t *testing.T) {
	want := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	for n, w := range want {
		if got := denyDelay(n); got != w {
			t.Fatalf("denyDelay(%d)=%v want %v", n, got, w)
		}
	}
}

func readyClock() <-chan time.Time {
	c := make(chan time.Time, 1)
	c <- time.Time{}
	return c
}

func attemptFrames(t *testing.T, in *bytes.Buffer, pins ...string) {
	t.Helper()
	for i, p := range pins {
		if err := WriteFrame(in, ports.AuthFrame{Kind: ports.AuthAttempt, Generation: 8, Attempt: uint64(i + 1), Payload: []byte(p)}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWrongPINDelaysGrowThenCapAndSuccessIsNotDelayed(t *testing.T) {
	var in, out bytes.Buffer
	attemptFrames(t, &in, "000001", "000002", "000003", "000004", "000005", "000006", "123456")
	clock := newMockdelayClock(t)
	for _, d := range []time.Duration{1, 2, 4, 8, 30, 30} {
		clock.EXPECT().after(d * time.Second).Return(readyClock()).Once()
	}
	// The right PIN is answered without any delay (no further clock call).
	if err := runWith(context.Background(), clock, &in, &out, 8, []byte("123456"), nil, "", false); err != nil {
		t.Fatal(err)
	}
}

func TestWrongPINDelayScheduleRestartsForAFreshWorker(t *testing.T) {
	// Success ends the worker, so the reset is observable as: the first wrong
	// answer of every fresh worker waits 1s again (no shared counter).
	for range 2 {
		var in, out bytes.Buffer
		attemptFrames(t, &in, "000001", "123456")
		clock := newMockdelayClock(t)
		clock.EXPECT().after(time.Second).Return(readyClock()).Once()
		if err := runWith(context.Background(), clock, &in, &out, 8, []byte("123456"), nil, "", false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDelayedDenialIsAnsweredOnlyAfterTheClockFires(t *testing.T) {
	inR, inW := io.Pipe()
	var out lockedBuffer
	fire := make(chan time.Time)
	clock := newMockdelayClock(t)
	clock.EXPECT().after(time.Second).Return((<-chan time.Time)(fire)).Once()
	done := make(chan error, 1)
	go func() { done <- runWith(context.Background(), clock, inR, &out, 8, []byte("123456"), nil, "", false) }()
	if err := WriteFrame(inW, ports.AuthFrame{Kind: ports.AuthAttempt, Generation: 8, Attempt: 1, Payload: []byte("000000")}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := out.frames(); n != 1 { // ready only: no denial yet
		t.Fatalf("%d frames before the delay elapsed", n)
	}
	fire <- time.Time{}
	if err := WriteFrame(inW, ports.AuthFrame{Kind: ports.AuthAttempt, Generation: 8, Attempt: 2, Payload: []byte("123456")}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := out.frames(); n != 3 {
		t.Fatalf("%d frames", n)
	}
}

func TestDelayEndsOnEOFAndCancellationWithoutAnswer(t *testing.T) {
	for _, name := range []string{"eof", "cancel"} {
		inR, inW := io.Pipe()
		var out lockedBuffer
		ctx, cancel := context.WithCancel(context.Background())
		clock := newMockdelayClock(t)
		clock.EXPECT().after(time.Second).Return((<-chan time.Time)(make(chan time.Time))).Once() // never fires
		done := make(chan error, 1)
		go func() { done <- runWith(ctx, clock, inR, &out, 8, []byte("123456"), nil, "", false) }()
		if err := WriteFrame(inW, ports.AuthFrame{Kind: ports.AuthAttempt, Generation: 8, Attempt: 1, Payload: []byte("000000")}); err != nil {
			t.Fatal(err)
		}
		want := io.EOF
		if name == "eof" {
			time.Sleep(20 * time.Millisecond)
			inW.Close()
		} else {
			want = context.Canceled
			time.Sleep(20 * time.Millisecond)
			cancel()
		}
		select {
		case err := <-done:
			if !errors.Is(err, want) {
				t.Fatalf("%s: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: delay did not end", name)
		}
		if n := out.frames(); n != 1 {
			t.Fatalf("%s: answered denial during the delay (%d frames)", name, n)
		}
		cancel()
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) frames() int {
	l.mu.Lock()
	cp := bytes.NewBuffer(bytes.Clone(l.b.Bytes()))
	l.mu.Unlock()
	n := 0
	for {
		if _, err := ReadFrame(cp); err != nil {
			return n
		}
		n++
	}
}

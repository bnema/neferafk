package visualproc

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/zerowrap"
)

// TestMain doubles as the child process: with NEFERAFK_TEST_CHILD set the test
// binary behaves like a tiny visual on fds 3/4 instead of running tests.
func TestMain(m *testing.M) {
	switch os.Getenv("NEFERAFK_TEST_CHILD") {
	case "":
		os.Exit(m.Run())
	case "echo": // confirm the lock it is asked for, then exit cleanly
		if os.Getenv("NEFERAFK_TEST_PIN") != "" {
			os.Exit(9) // the omitted variable leaked into the child
		}
		cmd, err := ReadCommand(os.NewFile(3, "cmd"))
		lock, ok := cmd.(ports.VisualLock)
		if err != nil || !ok || string(lock.Auth.EnvPIN) != "123456" {
			os.Exit(10)
		}
		if WriteEvent(os.NewFile(4, "ev"), ports.LockConfirmed{Generation: lock.Generation}) != nil {
			os.Exit(11)
		}
		os.Exit(0)
	case "crash":
		os.Exit(7)
	}
}

func launcher(mode string) *Launcher {
	exe, _ := os.Executable()
	return &Launcher{Executable: exe, Env: append(os.Environ(), "NEFERAFK_TEST_CHILD="+mode, "NEFERAFK_TEST_PIN=123456"), Log: zerowrap.New(zerowrap.Config{Output: io.Discard})}
}

func next(t *testing.T, s ports.VisualSession) ports.VisualEvent {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if !ok {
			t.Fatal("events closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return nil
	}
}

func TestLauncherPrivatePipesAndEnvOmission(t *testing.T) {
	s, err := launcher("echo").Launch(context.Background(), ports.VisualSpawn{OmitEnv: []string{"NEFERAFK_TEST_PIN"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send(ports.VisualLock{Generation: 5, Auth: ports.AuthBootstrap{Generation: 5, EnvPIN: []byte("123456")}}); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, s); ev != (ports.LockConfirmed{Generation: 5}) {
		t.Fatalf("%#v", ev)
	}
	ev := next(t, s)
	if x, ok := ev.(ports.VisualExited); !ok || x.Err != nil {
		t.Fatalf("clean exit reported as %#v", ev)
	}
	if _, ok := <-s.Events(); ok {
		t.Fatal("events not closed after exit")
	}
}

func TestLauncherReportsCrashAsExitNeverAsUnlock(t *testing.T) {
	s, err := launcher("crash").Launch(context.Background(), ports.VisualSpawn{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ev := next(t, s)
	if x, ok := ev.(ports.VisualExited); !ok || x.Err == nil {
		t.Fatalf("crash reported as %#v", ev)
	}
}

func TestLauncherDetachLeavesChildAndSecondCloseIsSafe(t *testing.T) {
	s, err := launcher("echo").Launch(context.Background(), ports.VisualSpawn{OmitEnv: []string{"NEFERAFK_TEST_PIN"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Detach(); err != nil {
		t.Fatal(err)
	}
	_ = s.Detach()
}

func TestLauncherLaunchFailure(t *testing.T) {
	if _, err := (&Launcher{Executable: "/nonexistent/neferafk", Log: zerowrap.New(zerowrap.Config{Output: io.Discard})}).Launch(context.Background(), ports.VisualSpawn{}); err == nil {
		t.Fatal("missing executable launched")
	}
}

// sleeper starts an unrelated process in its own group standing in for a
// reused pid/pgid.
func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "30")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	return c
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestCloseAfterReapNeverSignalsTheGroup(t *testing.T) {
	victim := sleeper(t)
	_, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	close(exited) // the visual was already reaped: its pgid may be reused
	s := &session{cmd: victim, w: w, exited: exited, log: zerowrap.New(zerowrap.Config{Output: io.Discard})}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !alive(victim.Process.Pid) {
		t.Fatal("Close signalled a process group after the child was reaped")
	}
}

func TestCloseBeforeReapKillsTheGroup(t *testing.T) {
	child := sleeper(t)
	_, w, _ := os.Pipe()
	s := &session{cmd: child, w: w, exited: make(chan struct{}), log: zerowrap.New(zerowrap.Config{Output: io.Discard})}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("live child group not killed")
	}
}

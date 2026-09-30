package main

import (
	"bytes"
	"context"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/bnema/neferafk/internal/adapters/auth"
	"github.com/bnema/neferafk/internal/ports"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // keep the daemon log out of the user's state
	var out, errb bytes.Buffer
	code := execute(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsageAndUnknownCommands(t *testing.T) {
	if code, _, e := run(t); code != exitUsage || !strings.Contains(e, "usage:") {
		t.Fatalf("%d %q", code, e)
	}
	if code, o, _ := run(t, "help"); code != exitOK || !strings.Contains(o, "lock") {
		t.Fatalf("%d %q", code, o)
	}
	// There is no unlock command, and the hidden worker is not advertised.
	if code, o, e := run(t, "unlock"); code != exitUsage || strings.Contains(o+e, "auth-worker") || !strings.Contains(e, "unknown command") {
		t.Fatalf("%d %q", code, e)
	}
	if strings.Contains(usage, "unlock") || strings.Contains(usage, "auth-worker") {
		t.Fatal("usage advertises a forbidden or hidden command")
	}
}

func TestVisualIsInternalAndTakesNoArguments(t *testing.T) {
	if code, _, e := run(t, "visual", "extra"); code != exitUsage || !strings.Contains(e, "internal command") {
		t.Fatalf("%d %q", code, e)
	}
}

func TestControlClientReportsMissingDaemon(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for _, c := range []string{"lock", "status"} {
		if code, _, e := run(t, c); code != exitFailure || !strings.Contains(e, "daemon not reachable") {
			t.Fatalf("%s: %d %q", c, code, e)
		}
		if code, _, _ := run(t, c, "extra"); code != exitUsage {
			t.Fatalf("%s accepted arguments", c)
		}
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if code, _, e := run(t, "status"); code != exitFailure || !strings.Contains(e, "XDG_RUNTIME_DIR") {
		t.Fatalf("%d %q", code, e)
	}
}

func TestRunReportsUnsupportedEnvironmentWithDistinctExit(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	code, _, e := run(t, "run", "--config", "/nonexistent/neferafk.conf")
	if code != exitUnsupported || !strings.Contains(e, "unsupported environment") || !strings.Contains(e, "WAYLAND_DISPLAY") {
		t.Fatalf("%d %q", code, e)
	}
	if code, _, _ := run(t, "run", "extra"); code != exitUsage {
		t.Fatal("run accepted positional arguments")
	}
	if code, _, _ := run(t, "run", "--bogus"); code != exitUsage {
		t.Fatal("run accepted an unknown flag")
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path := t.TempDir() + "/config"
	if err := writeFile(path, "lock.after = banana\n"); err != nil {
		t.Fatal(err)
	}
	if code, _, e := run(t, "run", "--config", path); code != exitFailure || !strings.Contains(e, "config") {
		t.Fatalf("%d %q", code, e)
	}
}

func TestServeAuthRunsWorkerOnPipes(t *testing.T) {
	var in, out, errb bytes.Buffer
	if err := auth.WriteBootstrap(&in, ports.AuthBootstrap{Generation: 3, PINSource: ports.PINSourceEnv, EnvPIN: []byte("123456")}); err != nil {
		t.Fatal(err)
	}
	if err := auth.WriteFrame(&in, ports.AuthFrame{Kind: ports.AuthAttempt, Generation: 3, Attempt: 1, Payload: []byte("123456")}); err != nil {
		t.Fatal(err)
	}
	if code := serveAuth(context.Background(), &in, &out, &errb); code != exitOK {
		t.Fatalf("%d %q", code, errb.String())
	}
	ready, err := auth.ReadFrame(&out)
	if err != nil || ready.Kind != ports.AuthReady || ready.Code != ports.AuthPIN {
		t.Fatalf("%+v %v", ready, err)
	}
	result, err := auth.ReadFrame(&out)
	if err != nil || result.Kind != ports.AuthResult || result.Code != ports.AuthSuccess {
		t.Fatalf("%+v %v", result, err)
	}
	// Garbage bootstrap: fail closed, no secret echoed.
	errb.Reset()
	if code := serveAuth(context.Background(), strings.NewReader("garbage-garbage"), &out, &errb); code != exitFailure || strings.Contains(errb.String(), "garbage") {
		t.Fatalf("%d %q", code, errb.String())
	}
}

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }

const cloexecChildEnv = "NEFERAFK_TEST_CLOEXEC_CHILD"

// TestInheritedPipesAreCloexec re-executes the test binary with two pipes as
// fds 3/4 exactly like the daemon spawns the worker and visual, and checks the
// child first sees them inheritable, then close-on-exec after the helper.
func TestInheritedPipesAreCloexec(t *testing.T) {
	if os.Getenv(cloexecChildEnv) == "1" {
		flag := func(fd int) int {
			v, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			if err != nil {
				t.Fatalf("fd %d: %v", fd, err)
			}
			return v & unix.FD_CLOEXEC
		}
		for _, fd := range []int{inheritedIn, inheritedOut} {
			if flag(fd) != 0 {
				t.Fatalf("fd %d already close-on-exec: the test proves nothing", fd)
			}
		}
		markInheritedCloexec()
		for _, fd := range []int{inheritedIn, inheritedOut} {
			if flag(fd) == 0 {
				t.Fatalf("fd %d not close-on-exec after markInheritedCloexec", fd)
			}
		}
		return
	}
	var files []*os.File
	for range 2 {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); w.Close() })
		files = append(files, r)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestInheritedPipesAreCloexec$")
	cmd.Env = append(os.Environ(), cloexecChildEnv+"=1")
	cmd.ExtraFiles = files
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

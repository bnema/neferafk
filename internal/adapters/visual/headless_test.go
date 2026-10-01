package visual

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bnema/nefergui"
	"github.com/stretchr/testify/require"
)

// The headless tests run the real lock and fade loops against a NeferWL
// compositor started with two virtual outputs in an isolated runtime
// directory. They are skipped unless NEFERAFK_HEADLESS names its binary, and
// they only ever connect to that compositor: WAYLAND_DISPLAY is set to its
// absolute socket path. Never point them at a real session.

var headlessEnvDrop = []string{"DISPLAY=", "WAYLAND_DISPLAY=", "WAYLAND_SOCKET=", "NOTIFY_SOCKET=", "DBUS_SESSION_BUS_ADDRESS=",
	"XDG_RUNTIME_DIR=", "XDG_CONFIG_HOME=", "XDG_DATA_HOME=", "XDG_STATE_HOME=", "XDG_SESSION_", "XDG_VTNR=", "XDG_SEAT=",
	"XDG_CURRENT_DESKTOP=", "NIRI_SOCKET=", "SWAYSOCK=", "HYPRLAND_INSTANCE_SIGNATURE=", "NEFERWL_"}

// startHeadless runs NeferWL, points WAYLAND_DISPLAY at it for this test and
// returns its input script writer.
func startHeadless(t *testing.T) io.Writer {
	t.Helper()
	bin := os.Getenv("NEFERAFK_HEADLESS")
	if bin == "" {
		t.Skip("set NEFERAFK_HEADLESS to a NeferWL binary")
	}
	bin, err := filepath.Abs(bin)
	require.NoError(t, err)
	root := t.TempDir()
	env := []string{}
	for _, e := range os.Environ() {
		keep := true
		for _, p := range headlessEnvDrop {
			keep = keep && !strings.HasPrefix(e, p)
		}
		if keep {
			env = append(env, e)
		}
	}
	run := filepath.Join(root, "run")
	for _, name := range []string{"run", "config", "data", "state"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, name), 0o700))
		env = append(env, "XDG_"+map[string]string{"run": "RUNTIME_DIR", "config": "CONFIG_HOME", "data": "DATA_HOME", "state": "STATE_HOME"}[name]+"="+filepath.Join(root, name))
	}
	logf, err := os.Create(filepath.Join(root, "neferwl.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = logf.Close() })
	cmd := exec.Command(bin, "--backend=headless", "--no-terminal", "--no-xwayland", "--size", "640x480,320x240",
		"--timeout", "60s", "--input", "-")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env, cmd.Stdout, cmd.Stderr = env, logf, logf
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	})
	for deadline := time.Now().Add(15 * time.Second); ; {
		paths, _ := filepath.Glob(filepath.Join(run, "wayland-*"))
		for _, p := range paths {
			if info, err := os.Stat(p); err == nil && info.Mode()&os.ModeSocket != 0 {
				require.True(t, filepath.IsAbs(p))
				t.Setenv("WAYLAND_DISPLAY", p) // absolute: XDG_RUNTIME_DIR is not used
				return stdin
			}
		}
		select {
		case <-exited:
			b, _ := os.ReadFile(logf.Name())
			t.Fatalf("neferwl exited before serving:\n%s", b)
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(logf.Name())
			t.Fatalf("headless socket timeout:\n%s", b)
		}
	}
}

// TestHeadlessLockSubmitsTypedSecretAndUnlocksOnRequest covers both outputs,
// waits for locked, types a secret, and checks that Enter submits exactly the
// typed bytes and that only the Unlock request ends the lock.
func TestHeadlessLockSubmitsTypedSecretAndUnlocksOnRequest(t *testing.T) {
	input := startHeadless(t)
	locked := make(chan struct{}, 1)
	submits := make(chan []byte, 1)
	unlock := make(chan struct{}, 1)
	status := make(chan lockStatus, 1)
	cfg := lockConfig{
		View:     func(f *nefergui.Frame, st lockState) { lockView(f, st, promptPassword, time.Now()) },
		OnLocked: func() { locked <- struct{}{} },
		OnSubmit: func(secret []byte) { submits <- append([]byte(nil), secret...) },
		OnOutputError: func(output string, err error) {
			t.Errorf("output %s: %v", output, err)
		},
		Unlock: unlock,
		Status: status,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- guiLocker{}.RunLock(ctx, cfg) }()

	recv(t, locked)
	_, err := io.WriteString(input, "sleep 200ms\ntype abc\nsleep 100ms\nkey BackSpace\ntype d\nsleep 100ms\nkey Return\n")
	require.NoError(t, err)
	require.Equal(t, []byte("abd"), recv(t, submits))
	status <- lockFailed // a status change only redraws
	select {
	case err := <-done:
		t.Fatalf("lock ended without an unlock request: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock <- struct{}{}
	require.NoError(t, recv(t, done))
}

// TestHeadlessFadeEndsAfterReveal fades every output to black, then back,
// and checks that the run ends by itself once the reveal completed.
func TestHeadlessFadeEndsAfterReveal(t *testing.T) {
	startHeadless(t)
	m := newFadeModel()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- guiFader{}.RunFade(ctx, m) }()
	m.send(fadeCmd{black: true, dur: 100 * time.Millisecond})
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("fade ended while black: %v", err)
	default:
	}
	m.send(fadeCmd{black: false, dur: 100 * time.Millisecond})
	require.NoError(t, recv(t, done))
}

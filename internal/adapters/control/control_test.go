package control

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/zerowrap"
)

func socketPath(t *testing.T) string {
	t.Helper()
	// Unix socket paths are short-limited: keep the temp dir shallow.
	dir, err := os.MkdirTemp("", "nc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "neferafk", "control.sock")
}

// start serves with allowed as the only accepted UID and answers requests with
// a channel-driven owner stand-in (a goroutine, not a project-interface double).
func start(t *testing.T, allowed uint32) (path string, requests <-chan ports.ControlKind) {
	t.Helper()
	path = socketPath(t)
	in := make(chan ports.ControlRequest)
	srv, err := Listen(path, allowed, in, zerowrap.New(zerowrap.Config{Output: io.Discard}))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan ports.ControlKind, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Serve(ctx); close(done) }()
	go func() {
		for {
			select {
			case r := <-in:
				seen <- r.Kind
				r.Reply <- ports.ControlReply{Generation: 3, Protected: r.Kind == ports.ControlLock}
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return path, seen
}

func TestPermissionsAndCommands(t *testing.T) {
	path, seen := start(t, uint32(os.Getuid()))
	di, _ := os.Stat(filepath.Dir(path))
	si, _ := os.Lstat(path)
	if di.Mode().Perm() != 0o700 || si.Mode().Perm() != 0o600 || si.Mode()&os.ModeSocket == 0 {
		t.Fatalf("dir %v socket %v", di.Mode(), si.Mode())
	}
	got, err := Do(context.Background(), path, "status")
	if err != nil || got != "ok generation=3 acquiring=false protected=false faded=false off=false" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := Do(context.Background(), path, "lock"); err != nil || !strings.Contains(got, "protected=true") {
		t.Fatalf("%q %v", got, err)
	}
	if k1, k2 := <-seen, <-seen; k1 != ports.ControlStatus || k2 != ports.ControlLock {
		t.Fatalf("%v %v", k1, k2)
	}
}

func TestUnknownCommandsRejectedNeverReachOwner(t *testing.T) {
	path, seen := start(t, uint32(os.Getuid()))
	for _, cmd := range []string{"unlock", "UNLOCK", "lock now", "lock ", " lock", "", "status\nunlock", strings.Repeat("a", 4096)} {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(2 * time.Second))
		io.WriteString(c, cmd+"\n")
		reply, _ := io.ReadAll(c)
		c.Close()
		if string(reply) != "err unknown command\n" && !(cmd == "status\nunlock" && strings.HasPrefix(string(reply), "ok ")) {
			t.Fatalf("%q -> %q", cmd, reply)
		}
	}
	// "status\nunlock" is a valid status line followed by ignored input.
	if k := <-seen; k != ports.ControlStatus {
		t.Fatalf("kind %v", k)
	}
	select {
	case k := <-seen:
		t.Fatalf("unexpected request %v", k)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := Do(context.Background(), path, "unlock"); err == nil {
		t.Fatal("client got success for unlock")
	}
}

func TestForeignUIDRejectedWithoutReply(t *testing.T) {
	// Only uid+1 is allowed, so this process's own credentials are foreign.
	path, seen := start(t, uint32(os.Getuid())+1)
	if got, err := Do(context.Background(), path, "lock"); err == nil {
		t.Fatalf("foreign peer served: %q", got)
	}
	select {
	case k := <-seen:
		t.Fatalf("foreign request reached owner: %v", k)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestListenRefusesLiveDaemonReplacesStaleSocketAndRejectsNonSocket(t *testing.T) {
	path, _ := start(t, uint32(os.Getuid()))
	if _, err := Listen(path, uint32(os.Getuid()), make(chan ports.ControlRequest), zerowrap.New(zerowrap.Config{Output: io.Discard})); err == nil {
		t.Fatal("second daemon took the live socket")
	}
	stale := socketPath(t)
	os.MkdirAll(filepath.Dir(stale), 0o700)
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	srv, err := Listen(stale, uint32(os.Getuid()), make(chan ports.ControlRequest), zerowrap.New(zerowrap.Config{Output: io.Discard}))
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	srv.ln.Close()
	file := socketPath(t)
	os.MkdirAll(filepath.Dir(file), 0o700)
	os.WriteFile(file, nil, 0o600)
	if _, err := Listen(file, uint32(os.Getuid()), make(chan ports.ControlRequest), zerowrap.New(zerowrap.Config{Output: io.Discard})); err == nil {
		t.Fatal("regular file replaced")
	}
}

func TestListenTightensLooseDirectoryAndRejectsForeignDirectory(t *testing.T) {
	path := socketPath(t)
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.Chmod(filepath.Dir(path), 0o755)
	srv, err := Listen(path, uint32(os.Getuid()), make(chan ports.ControlRequest), zerowrap.New(zerowrap.Config{Output: io.Discard}))
	if err != nil {
		t.Fatal(err)
	}
	srv.ln.Close()
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode())
	}
	// A directory that is not ours (root's /proc) is refused.
	if _, err := Listen("/proc/neferafk/control.sock", uint32(os.Getuid()), make(chan ports.ControlRequest), zerowrap.New(zerowrap.Config{Output: io.Discard})); err == nil {
		t.Fatal("foreign directory accepted")
	}
}

func TestDefaultPathRequiresAbsoluteRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := DefaultPath(); err == nil {
		t.Fatal("empty runtime dir accepted")
	}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if p, err := DefaultPath(); err != nil || p != "/run/user/1000/neferafk/control.sock" {
		t.Fatalf("%q %v", p, err)
	}
}

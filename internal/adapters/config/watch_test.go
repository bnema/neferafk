package config

import (
	"context"
	"github.com/bnema/neferafk/internal/ports"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("watch timeout")
		var z T
		return z
	}
}
func save(t *testing.T, path, text string) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}
func TestWatchAtomicInvalidTransactionalAndSymlinkRetarget(t *testing.T) {
	dir := t.TempDir()
	targetDir := t.TempDir()
	path := filepath.Join(dir, "config")
	target := filepath.Join(targetDir, "target")
	save(t, path, "lock.after=5m\n")
	out := make(chan ports.ConfigChanged, 4)
	errs := make(chan error, 4)
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, path, out, errs, ready) }()
	recv(t, ready)
	if initial := recv(t, out); initial.Config.LockAfter != 5*time.Minute {
		t.Fatal(initial)
	}
	save(t, path, "lock.after=2m\n")
	if got := recv(t, out); got.Config.LockAfter != 2*time.Minute {
		t.Fatal(got)
	}
	save(t, path, "lock.after=-1s\n")
	recv(t, errs)
	select {
	case v := <-out:
		t.Fatalf("invalid reload emitted %+v", v)
	default:
	}
	save(t, target, "lock.after=3m\n")
	if err := os.Symlink(target, path+".link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".link", path); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, out); got.Config.LockAfter != 3*time.Minute {
		t.Fatal(got)
	}
	save(t, target, "lock.after=4m\n")
	if got := recv(t, out); got.Config.LockAfter != 4*time.Minute {
		t.Fatal(got)
	}
	cancel()
	if err := recv(t, done); err != nil {
		t.Fatal(err)
	}
}
func TestWatchCancellationWhileDeliveryBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	save(t, path, "lock.after=5m\n")
	out := make(chan ports.ConfigChanged)
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, path, out, nil, ready) }()
	recv(t, ready)
	save(t, path, "lock.after=1m\n")
	cancel()
	if err := recv(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestWatchMissingDirectoryStartupThenRemoveRecreate(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "missing", "nested")
	path := filepath.Join(dir, "config")
	out := make(chan ports.ConfigChanged, 4)
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, path, out, nil, ready) }()
	recv(t, ready)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	save(t, path, "lock.after=1m\n")
	if got := recv(t, out); got.Config.LockAfter != time.Minute {
		t.Fatal(got)
	}
	// Rename the entire watched directory out of the path, then recreate it.
	if err := os.Rename(dir, dir+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	save(t, path, "lock.after=2m\n")
	if got := recv(t, out); got.Config.LockAfter != 2*time.Minute {
		t.Fatal(got)
	}
	cancel()
	if err := recv(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestWatchRetargetToUnavailableThenReappearKeepsLastValid(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config")
	save(t, path, "lock.after=1m\n")
	out := make(chan ports.ConfigChanged, 4)
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, path, out, nil, ready) }()
	recv(t, ready)
	recv(t, out)
	target := filepath.Join(t.TempDir(), "absent", "entry")
	if err := os.Symlink(target, path+".link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".link", path); err != nil {
		t.Fatal(err)
	}
	select {
	case reset := <-out:
		t.Fatalf("missing target reset last valid %+v", reset)
	case err := <-done:
		t.Fatalf("watch stopped on missing target %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	save(t, target, "lock.after=3m\n")
	if got := recv(t, out); got.Config.LockAfter != 3*time.Minute {
		t.Fatal(got)
	}
	cancel()
	if err := recv(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestWatchFirstCandidateClosesLoadStartupGapAndMissingCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	save(t, path, "lock.after=1m\n")
	old, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	save(t, path, "lock.after=2m\n") // save after app's Load, before Watch
	out := make(chan ports.ConfigChanged, 1)
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, path, out, nil, ready) }()
	recv(t, ready)
	if got := recv(t, out); got.Config == old || got.Config.LockAfter != 2*time.Minute {
		t.Fatal(got)
	}
	cancel()
	if err := recv(t, done); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan error, 1)
	ready = make(chan struct{})
	go func() { done <- Watch(ctx, filepath.Join(t.TempDir(), "never", "config"), out, nil, ready) }()
	recv(t, ready)
	cancel()
	if err := recv(t, done); err != nil {
		t.Fatal(err)
	}
}

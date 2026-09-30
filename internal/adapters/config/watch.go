package config

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	"golang.org/x/sys/unix"
)

func watchedFiles(path string) []string {
	files := []string{filepath.Clean(path)}
	for len(files) <= 40 {
		cur := files[len(files)-1]
		target, err := os.Readlink(cur)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		target = filepath.Clean(target)
		duplicate := false
		for _, p := range files {
			if p == target {
				duplicate = true
				break
			}
		}
		if duplicate {
			break
		}
		files = append(files, target)
	}
	return files
}

// watchDirectory finds the nearest existing ancestor. The missing component's
// creation wakes inotify; the two-second retry also covers lost/overflow events.
func watchDirectory(file string) (directory, name string, missing bool) {
	directory, name = filepath.Dir(file), filepath.Base(file)
	for {
		st, err := os.Stat(directory)
		if err == nil && st.IsDir() {
			return
		}
		missing = true
		parent := filepath.Dir(directory)
		if parent == directory {
			return
		}
		name, directory = filepath.Base(directory), parent
	}
}

// Watch retains last-valid state across missing files/directories and follows
// config symlink targets. It always emits the first valid candidate, including
// a candidate equal to defaults: this closes an app Load -> Watch startup gap.
// Ready closes after initial watch installation, before candidate delivery.
// No invalid/deleted candidate emits a defaults reset. Errors never echo file
// content or paths, which may themselves contain sensitive source metadata.
func Watch(ctx context.Context, path string, out chan<- ports.ConfigChanged, failures chan<- error, ready chan<- struct{}) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return errors.New("initialize config watch")
	}
	defer unix.Close(fd)
	const mask = unix.IN_CLOSE_WRITE | unix.IN_MODIFY | unix.IN_MOVED_TO | unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF
	names := map[int32]map[string]bool{}
	retry := time.Time{}
	rewatch := func() {
		// Keep unchanged watches, avoiding IN_IGNORED from our own removals.
		want := map[int32]map[string]bool{}
		retry = time.Time{}
		for _, file := range watchedFiles(path) {
			dir, name, missing := watchDirectory(file)
			wd, e := unix.InotifyAddWatch(fd, dir, mask)
			if e != nil {
				retry = time.Now().Add(2 * time.Second)
				continue
			}
			if missing {
				retry = time.Now().Add(2 * time.Second)
			}
			if want[int32(wd)] == nil {
				want[int32(wd)] = map[string]bool{}
			}
			want[int32(wd)][name] = true
		}
		for wd := range names {
			if want[wd] == nil {
				_, _ = unix.InotifyRmWatch(fd, uint32(wd))
			}
		}
		names = want
	}
	rewatch()
	if ready != nil {
		close(ready)
	}
	var last ports.Config
	haveLast := false
	publish := func() bool {
		cfg, e := Load(path)
		if e != nil {
			if !os.IsNotExist(e) && failures != nil {
				select {
				case failures <- errors.New("config reload rejected"):
				default:
				}
			}
			return true
		}
		if haveLast && cfg == last {
			return true
		}
		select {
		case out <- ports.ConfigChanged{Config: cfg}:
			last, haveLast = cfg, true
			return true
		case <-ctx.Done():
			return false
		}
	}
	if !publish() {
		return nil
	}
	pending := time.Time{}
	buf := make([]byte, 8192)
	for ctx.Err() == nil {
		now := time.Now()
		if !retry.IsZero() && !now.Before(retry) {
			rewatch()
			pending = now
		}
		if !pending.IsZero() && !now.Before(pending) {
			pending = time.Time{}
			rewatch()
			if !publish() {
				return nil
			}
		}
		timeout := 100 // cancellation bound, no periodic reads while watches are live
		if !pending.IsZero() {
			timeout = min(timeout, max(1, int(time.Until(pending).Milliseconds())))
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, e := unix.Poll(fds, timeout)
		if errors.Is(e, unix.EINTR) {
			continue
		}
		if e != nil {
			return errors.New("poll config watch")
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, e := unix.Read(fd, buf)
		if errors.Is(e, unix.EAGAIN) || errors.Is(e, unix.EINTR) {
			continue
		}
		if e != nil {
			return errors.New("read config watch")
		}
		for offset := 0; offset+unix.SizeofInotifyEvent <= n; {
			b := buf[offset:]
			wd := int32(binary.NativeEndian.Uint32(b))
			flags := binary.NativeEndian.Uint32(b[4:])
			length := int(binary.NativeEndian.Uint32(b[12:]))
			size := unix.SizeofInotifyEvent + length
			if size > n-offset {
				break
			}
			if flags&unix.IN_Q_OVERFLOW != 0 {
				pending = time.Now().Add(100 * time.Millisecond)
			}
			if names[wd] != nil {
				if flags&(unix.IN_DELETE_SELF|unix.IN_MOVE_SELF|unix.IN_IGNORED) != 0 {
					if flags&unix.IN_IGNORED == 0 {
						_, _ = unix.InotifyRmWatch(fd, uint32(wd))
					}
					delete(names, wd)
					pending = time.Now().Add(100 * time.Millisecond)
					retry = time.Now().Add(2 * time.Second)
				}
				name := strings.TrimRight(string(b[unix.SizeofInotifyEvent:size]), "\x00")
				if names[wd][name] {
					pending = time.Now().Add(100 * time.Millisecond)
				}
			}
			offset += size
		}
	}
	return nil
}

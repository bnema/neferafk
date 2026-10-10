package config

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/bnema/kvconf"
	"github.com/bnema/neferafk/internal/ports"
)

// fallbackPoll is the polling interval used when native watching fails.
var fallbackPoll = 2 * time.Second

// kvWatch is kvconf.Watch; tests replace it to make native watching fail.
var kvWatch = kvconf.Watch

// Watch retains last-valid state across missing files/directories and follows
// config symlink targets (kvconf.Watch). It always emits the first valid
// candidate, including a candidate equal to defaults: this closes an app
// Load -> Watch startup gap. Ready closes after initial watch installation,
// before candidate delivery. No invalid/deleted candidate emits a defaults
// reset. Errors never echo file content or paths, which may themselves
// contain sensitive source metadata. When the OS refuses to watch (inotify
// limits, permissions), Watch reports it on failures and polls instead: a
// watch error must never stop the daemon and with it idle locking.
func Watch(ctx context.Context, path string, out chan<- ports.ConfigChanged, failures chan<- error, ready chan<- struct{}) error {
	ctx, cancel := context.WithCancel(ctx)
	changed := make(chan struct{}, 1)
	installed := make(chan struct{})
	done := make(chan struct{})
	var watchErr error
	go func() {
		defer close(done)
		err := kvWatch(ctx, path, kvconf.WatchOptions{Ready: installed}, changed)
		if err == nil || ctx.Err() != nil {
			return
		}
		if failures != nil {
			select {
			case failures <- errors.New("config watch failed; polling"):
			default:
			}
		}
		opts := kvconf.WatchOptions{Poll: fallbackPoll}
		select {
		case <-installed: // closed by the native watch before it failed
		default:
			opts.Ready = installed
		}
		// Changes made while nothing watched are seen by this reload.
		select {
		case changed <- struct{}{}:
		default:
		}
		watchErr = kvWatch(ctx, path, opts, changed)
	}()
	defer func() { cancel(); <-done }()

	select {
	case <-installed:
	case <-done:
		if watchErr != nil {
			return errors.New("initialize config watch")
		}
		return nil
	}
	if ready != nil {
		close(ready)
	}
	var last ports.Config
	haveLast := false
	publish := func() bool {
		cfg, err := Load(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) && failures != nil {
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
	for ok := publish(); ok; {
		select {
		case <-ctx.Done():
			return nil
		case <-done:
			if watchErr != nil {
				return errors.New("config watch")
			}
			return nil
		case <-changed:
			ok = publish()
		}
	}
	return nil
}

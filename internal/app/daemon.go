package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/bnema/neferafk/internal/adapters/config"
	"github.com/bnema/neferafk/internal/adapters/control"
	"github.com/bnema/neferafk/internal/adapters/logind"
	"github.com/bnema/neferafk/internal/adapters/visualproc"
	"github.com/bnema/neferafk/internal/adapters/wayland"
	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/zerowrap"
)

// DaemonOptions configures RunDaemon.
type DaemonOptions struct {
	ConfigPath string // empty: config.DefaultPath()
	// Executable is re-executed as `<Executable> visual` for the visual process.
	Executable string
	Log        zerowrap.Logger
}

// RunDaemon wires the real adapters around one Owner. It returns nil on a
// requested shutdown, *UnsupportedError for missing capabilities, or the first
// fatal adapter error. Nothing in the daemon initialises Vulkan or fonts.
func RunDaemon(ctx context.Context, o DaemonOptions) (result error) {
	log := o.Log.WithField("component", "daemon")
	path := o.ConfigPath
	if path == "" {
		path = config.DefaultPath()
	}
	cfg, err := config.Load(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg = ports.Defaults()
	case err != nil:
		return fmt.Errorf("config: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wlReq, sysReq := Requirements(cfg)
	conn, err := dialWayland()
	if err != nil {
		return &UnsupportedError{Missing: []string{"wayland: " + err.Error()}}
	}
	// Until the adapters own their resources (started), every early return
	// releases the compositor connection and logind, joining Close errors.
	started := false
	var sys *logind.Adapter
	defer func() {
		if started {
			return
		}
		if e := conn.Close(); e != nil && !errors.Is(e, net.ErrClosed) {
			result = errors.Join(result, fmt.Errorf("close wayland connection: %w", e))
		}
		if sys != nil {
			if e := sys.Close(); e != nil {
				result = errors.Join(result, fmt.Errorf("close logind: %w", e))
			}
		}
	}()
	// Initial deadlines come from the owner's first ArmIdle (SetDeadlines).
	wl, err := wayland.New(conn, wayland.Options{Requirements: wlReq, Generation: 1}, o.Log)
	if err != nil {
		return fmt.Errorf("wayland: %w", err)
	}
	sys, err = logind.Open(ctx, sysReq)
	if err != nil {
		sys = nil
		return &UnsupportedError{Missing: []string{"logind: " + err.Error()}}
	}

	uid := uint32(os.Getuid())
	sockPath, err := control.DefaultPath()
	if err != nil {
		return err
	}
	controlCh := make(chan ports.ControlRequest, 8)
	srv, err := control.Listen(sockPath, uid, controlCh, o.Log)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}

	cfgCh := make(chan ports.ConfigChanged, 4)
	failCh := make(chan error, 4)
	wlEvents := make(chan ports.WaylandEvent, 64)
	wlCmds := make(chan ports.WaylandCommand, 16)
	sysEvents := make(chan ports.SystemEvent, 16)
	sysCmds := make(chan ports.SystemCommand, 16)
	visEvents := make(chan ports.VisualEvent, 16)

	launcher := &visualproc.Launcher{Executable: o.Executable, Args: []string{"visual"}, Log: o.Log}
	visual := visualproc.NewManager(ctx, launcher, visEvents, o.Log)
	owner, err := NewOwner(Options{Config: cfg, Visual: visual, Log: o.Log, Gate: true, ExpectSystem: true},
		Inputs{Config: cfgCh, Failures: failCh, Wayland: wlEvents, System: sysEvents, Visual: visEvents, Control: controlCh},
		Outputs{Wayland: wlCmds, System: sysCmds})
	if err != nil {
		return errors.Join(err, srv.Close())
	}
	started = true

	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	fail := func(name string, err error) {
		if err == nil || ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return
		}
		mu.Lock()
		if first == nil {
			first = fmt.Errorf("%s: %w", name, err)
		}
		mu.Unlock()
		cancel()
	}
	spawn := func(name string, fn func() error) {
		wg.Add(1)
		go func() { defer wg.Done(); fail(name, fn()) }()
	}
	spawn("config watch", func() error { return config.Watch(ctx, path, cfgCh, failCh, nil) })
	spawn("wayland", func() error { return wl.Run(ctx, wlCmds, wlEvents) })
	spawn("logind", func() error { return sys.Run(ctx, sysCmds, sysEvents) })
	spawn("control", func() error { return srv.Serve(ctx) })

	ownerErr := owner.Run(ctx)
	cancel()
	wg.Wait()
	log.Info().Msg("daemon stopped")
	var unsup *UnsupportedError
	if errors.As(ownerErr, &unsup) {
		return ownerErr
	}
	mu.Lock()
	defer mu.Unlock()
	if ownerErr != nil {
		return ownerErr
	}
	return first
}

func dialWayland() (net.Conn, error) {
	name := os.Getenv("WAYLAND_DISPLAY")
	if name == "" {
		return nil, errors.New("WAYLAND_DISPLAY is not set")
	}
	if !filepath.IsAbs(name) {
		dir := os.Getenv("XDG_RUNTIME_DIR")
		if dir == "" {
			return nil, errors.New("XDG_RUNTIME_DIR is not set")
		}
		name = filepath.Join(dir, name)
	}
	c, err := net.Dial("unix", name)
	if err != nil {
		return nil, fmt.Errorf("connect compositor: %w", err)
	}
	return c, nil
}

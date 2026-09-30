// Package logind observes the current process's session and owns bounded
// logind calls/inhibitor descriptors. It contains no lock or sleep policy.
package logind

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const service = "org.freedesktop.login1"
const managerPath dbus.ObjectPath = "/org/freedesktop/login1"
const managerInterface = "org.freedesktop.login1.Manager"
const sessionInterface = "org.freedesktop.login1.Session"
const callTimeout = 3 * time.Second

// Adapter is single-owner: Probe, Run and Close must not execute concurrently.
// Run owns the connection and closes it and any inhibitor on every exit.
type Adapter struct {
	bus          bus
	requirements ports.SystemRequirements
	owner        string
	session      dbus.ObjectPath
	caps         ports.SystemCapabilities
	signals      chan *dbus.Signal
	inhibitor    *os.File
	cycle        uint64
	preparing    bool
	closed       bool
}

// Open connects only when requested. Calling it is unnecessary when all system
// features are off. Connection failure is an unsupported capability, not a
// reason to force system initialization at daemon startup.
func Open(ctx context.Context, requirements ports.SystemRequirements) (*Adapter, error) {
	if !requirements.Session && !requirements.Sleep {
		return nil, errors.New("logind not requested")
	}
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("system bus unavailable: %w", err)
	}
	a := newAdapter(connection{conn}, requirements)
	if err := a.Probe(ctx); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}
func newAdapter(b bus, r ports.SystemRequirements) *Adapter {
	return &Adapter{bus: b, requirements: r, signals: make(chan *dbus.Signal, 32)}
}
func (a *Adapter) call(ctx context.Context, path dbus.ObjectPath, method string, args ...any) ([]any, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return a.bus.call(ctx, a.owner, path, method, args...)
}
func (a *Adapter) property(ctx context.Context, path dbus.ObjectPath, iface, name string) (any, error) {
	body, err := a.call(ctx, path, "org.freedesktop.DBus.Properties.Get", iface, name)
	if err != nil {
		return nil, err
	}
	if len(body) != 1 {
		return nil, errors.New("invalid property reply")
	}
	v, ok := body[0].(dbus.Variant)
	if !ok {
		return nil, errors.New("invalid property variant")
	}
	return v.Value(), nil
}

// Probe is read-only: no Lock/Unlock/Suspend/Inhibit call. It resolves the
// unique service owner, PID's session and its real UID; no user override exists.
func (a *Adapter) Probe(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	body, err := a.bus.call(bounded, "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus.GetNameOwner", service)
	if err != nil {
		return fmt.Errorf("logind unavailable: %w", err)
	}
	if len(body) != 1 {
		return errors.New("invalid logind owner reply")
	}
	owner, ok := body[0].(string)
	if !ok || !strings.HasPrefix(owner, ":") {
		return errors.New("invalid unique logind owner")
	}
	a.owner = owner
	a.caps = ports.SystemCapabilities{}
	if a.requirements.Session {
		body, err = a.call(ctx, managerPath, managerInterface+".GetSessionByPID", uint32(os.Getpid()))
		if err != nil {
			return fmt.Errorf("current process session unavailable: %w", err)
		}
		if len(body) != 1 {
			return errors.New("invalid session reply")
		}
		path, ok := body[0].(dbus.ObjectPath)
		if !ok || !path.IsValid() || !strings.HasPrefix(string(path), "/org/freedesktop/login1/session/") {
			return errors.New("invalid session path")
		}
		value, err := a.property(ctx, path, sessionInterface, "User")
		if err != nil {
			return err
		}
		fields, ok := value.([]any)
		if !ok || len(fields) != 2 {
			return errors.New("invalid session User property")
		}
		uid, ok := fields[0].(uint32)
		if !ok || uid != uint32(os.Getuid()) {
			return errors.New("session does not belong to real UID")
		}
		a.session = path
		a.caps.Session = true
	}
	if a.requirements.Sleep {
		body, err = a.call(ctx, managerPath, managerInterface+".CanSuspend")
		if err != nil {
			return fmt.Errorf("suspend capability unavailable: %w", err)
		}
		if len(body) != 1 {
			return errors.New("invalid CanSuspend reply")
		}
		auth, ok := body[0].(string)
		if !ok {
			return errors.New("invalid CanSuspend type")
		}
		a.caps.SuspendAuthorization = auth
		// Suspend(false) cannot request an interactive authorization prompt.
		a.caps.Suspend = auth == "yes"
		if !a.caps.Suspend {
			a.caps.Missing = append(a.caps.Missing, "non-interactive suspend: "+auth)
		}
		value, err := a.property(ctx, managerPath, managerInterface, "InhibitDelayMaxUSec")
		if err != nil {
			return err
		}
		usec, ok := value.(uint64)
		if !ok || usec == 0 || usec > uint64(math.MaxInt64/1000) {
			return errors.New("invalid inhibitor delay maximum")
		}
		a.caps.DelayMax = time.Duration(usec) * time.Microsecond
		if !a.bus.unixFDs() {
			a.caps.Missing = append(a.caps.Missing, "D-Bus Unix FD passing")
			a.caps.Suspend = false
		}
	}
	return nil
}
func (a *Adapter) Capabilities() ports.SystemCapabilities {
	c := a.caps
	c.Missing = append([]string(nil), c.Missing...)
	return c
}
func (a *Adapter) emit(ctx context.Context, out chan<- ports.SystemEvent, event ports.SystemEvent) error {
	select {
	case out <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run starts subscriptions and holds a delay inhibitor only for enabled sleep.
// A matching SleepReady releases it. Resume reacquires for the next cycle.
// logind's own maximum bounds the delay even if policy never replies.
func (a *Adapter) Run(ctx context.Context, commands <-chan ports.SystemCommand, events chan<- ports.SystemEvent) error {
	defer a.Close()
	if a.owner == "" {
		return errors.New("logind must be probed before Run")
	}
	a.bus.signals(a.signals)
	defer a.bus.removeSignals(a.signals)
	paths := []struct {
		path  dbus.ObjectPath
		iface string
	}{}
	if a.requirements.Session {
		paths = append(paths, struct {
			path  dbus.ObjectPath
			iface string
		}{a.session, sessionInterface})
	}
	if a.requirements.Sleep {
		paths = append(paths, struct {
			path  dbus.ObjectPath
			iface string
		}{managerPath, managerInterface})
	}
	for _, p := range paths {
		rule := fmt.Sprintf("type='signal',sender='%s',path='%s',interface='%s'", a.owner, p.path, p.iface)
		bounded, cancel := context.WithTimeout(ctx, callTimeout)
		err := a.bus.match(bounded, rule)
		cancel()
		if err != nil {
			return err
		}
	}
	if a.requirements.Sleep && a.bus.unixFDs() {
		if err := a.inhibit(ctx); err != nil {
			a.caps.Missing = append(a.caps.Missing, "sleep delay inhibitor")
			a.caps.Suspend = false
		}
	}
	if err := a.emit(ctx, events, ports.SystemCapabilityReport{Capabilities: a.Capabilities()}); err != nil {
		return err
	}
	var suspendReply <-chan *dbus.Call
	var suspendCancel context.CancelFunc
	defer func() {
		if suspendCancel != nil {
			suspendCancel()
		}
	}()
	for {
		select {
		case reply := <-suspendReply:
			suspendCancel()
			suspendReply, suspendCancel = nil, nil
			if reply == nil {
				return errors.New("suspend reply channel closed")
			}
			if reply.Err != nil {
				if err := a.emit(ctx, events, ports.SystemFailure{Operation: "suspend", Err: reply.Err}); err != nil {
					return err
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		case sig, ok := <-a.signals:
			if !ok {
				return errors.New("system bus signal stream closed")
			}
			event, ok := parseSignal(sig, a.owner, a.session)
			if !ok {
				continue
			}
			if prep, ok := event.(ports.SleepPreparation); ok {
				if prep.Preparing && !a.preparing {
					a.cycle++
				}
				a.preparing = prep.Preparing
				prep.Cycle, prep.MaxDelay, prep.ReceivedAt = a.cycle, a.caps.DelayMax, time.Now()
				event = prep
				if !prep.Preparing {
					a.release()
					if err := a.inhibit(ctx); err != nil {
						if e := a.emit(ctx, events, ports.SystemFailure{Operation: "inhibit", Err: err}); e != nil {
							return e
						}
					}
				}
			}
			if err := a.emit(ctx, events, event); err != nil {
				return err
			}
		case cmd, ok := <-commands:
			if !ok {
				return nil
			}
			switch c := cmd.(type) {
			case ports.SystemSleepReady:
				if a.preparing && c.Cycle == a.cycle {
					a.release()
				}
			case ports.SystemSuspend:
				var err error
				if !a.requirements.Sleep || !a.caps.Suspend {
					err = errors.New("non-interactive suspend unsupported")
				} else if suspendReply != nil {
					err = errors.New("suspend request already pending")
				} else {
					// Never override block inhibitors, including ones owned by
					// this UID (which privileged logind policies may otherwise
					// permit). Query immediately before the ordinary Suspend call.
					blocked, e := a.property(ctx, managerPath, managerInterface, "BlockInhibited")
					if e != nil {
						err = e
					} else if value, ok := blocked.(string); !ok {
						err = errors.New("invalid block inhibitor property")
					} else {
						for _, item := range strings.Split(value, ":") {
							if item == "sleep" {
								err = errors.New("sleep blocked by inhibitor")
							}
						}
					}
					if err != nil {
						if e := a.emit(ctx, events, ports.SystemFailure{Operation: "suspend", Err: err}); e != nil {
							return e
						}
						continue
					}
					bounded, cancel := context.WithTimeout(ctx, callTimeout)
					suspendCancel = cancel // released on reply or by the loop's defer
					suspendReply = a.bus.suspend(bounded, a.owner)
				}
				if err != nil {
					if e := a.emit(ctx, events, ports.SystemFailure{Operation: "suspend", Err: err}); e != nil {
						return e
					}
				}
			}
		}
	}
}

func parseSignal(sig *dbus.Signal, owner string, session dbus.ObjectPath) (ports.SystemEvent, bool) {
	if sig == nil || sig.Sender != owner {
		return nil, false
	}
	if sig.Path == session && session != "" && len(sig.Body) == 0 {
		switch sig.Name {
		case sessionInterface + ".Lock":
			return ports.SystemLockRequested{}, true
		case sessionInterface + ".Unlock":
			return ports.SystemUnlockObserved{}, true
		}
	}
	if sig.Path == managerPath && sig.Name == managerInterface+".PrepareForSleep" && len(sig.Body) == 1 {
		preparing, ok := sig.Body[0].(bool)
		if ok {
			return ports.SleepPreparation{Preparing: preparing}, true
		}
	}
	return nil, false
}

func (a *Adapter) inhibit(ctx context.Context) error {
	if !a.requirements.Sleep || a.inhibitor != nil {
		return nil
	}
	body, err := a.call(ctx, managerPath, managerInterface+".Inhibit", "sleep", "neferafk", "session protection", "delay")
	if err != nil {
		return err
	}
	if len(body) != 1 {
		for _, value := range body {
			if fd, ok := value.(dbus.UnixFD); ok && fd >= 0 {
				_ = unix.Close(int(fd))
			}
		}
		return errors.New("invalid inhibitor reply")
	}
	fd, ok := body[0].(dbus.UnixFD)
	if !ok || fd < 0 {
		return errors.New("invalid inhibitor descriptor")
	}
	// godbus returns the received descriptor directly, not an os.File and not
	// an owned duplicate. Atomically duplicate CLOEXEC then close the received FD.
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	_ = unix.Close(int(fd))
	if err != nil {
		return err
	}
	a.inhibitor = os.NewFile(uintptr(dup), "logind-sleep-delay")
	return nil
}
func (a *Adapter) release() {
	if a.inhibitor != nil {
		_ = a.inhibitor.Close()
		a.inhibitor = nil
	}
}
func (a *Adapter) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	a.release()
	return a.bus.close()
}

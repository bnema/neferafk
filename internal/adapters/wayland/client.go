// Package wayland owns a standard-protocol control connection. It never owns
// lock surfaces or grabs input; visual processes own ext-session-lock.
package wayland

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/wlturbo"
	"github.com/bnema/wlturbo/protocol/core"
	"github.com/bnema/wlturbo/protocol/idlenotify"
	"github.com/bnema/wlturbo/protocol/outputpower"
	"github.com/bnema/zerowrap"
)

const queueSize = 64

var ErrEventOverflow = errors.New("wayland: event queue overflow")

type Options struct {
	Requirements ports.WaylandRequirements
	Deadlines    []time.Duration
	Generation   uint64
}
type notice struct {
	kind         uint8
	notification *notification
	output       *output
	name         string
	mode         uint32
}
type notification struct {
	proxy                    *idlenotify.ExtIdleNotification
	token, cycle, generation uint64
	seat                     uint32
	after                    time.Duration
	active, wake, idled      bool
}
type seat struct {
	proxy         *core.Seat
	notifications map[time.Duration]*notification
	wake          *notification
}
type output struct {
	proxy       *core.Output
	power       *outputpower.OutputPower
	lifetime    uint64
	global      uint32
	name        string
	off, failed bool
	mode        uint32
	modeKnown   bool
}
type Client struct {
	d                *wlturbo.Display
	options          Options
	log              zerowrap.Logger
	notices          chan notice
	overflow         chan struct{}
	seats            map[uint32]*seat
	outputs          map[uint32]*output
	idle             *idlenotify.ExtIdleNotifier
	idleGlobal       uint32
	power            *outputpower.OutputPowerManager
	powerGlobal      uint32
	deadlines        map[time.Duration]bool
	generation, next uint64
	dirty            bool
}

// New takes ownership of conn. Run is called exactly once; no other reader or
// caller may use the display. Supplying an already connected socket permits
// real socketpair tests without a replacement transport.
func New(conn net.Conn, options Options, log zerowrap.Logger) (*Client, error) {
	deadlines, err := normalize(options.Deadlines)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	d, err := wlturbo.ConnectFromConn(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	c := &Client{d: d, options: options, log: log.WithField("component", "wayland"), notices: make(chan notice, queueSize), overflow: make(chan struct{}, 1), seats: map[uint32]*seat{}, outputs: map[uint32]*output{}, deadlines: deadlines, generation: options.Generation}
	d.Registry().AddHandler("*", func(*wlturbo.Registry, uint32, uint32) { c.enqueue(notice{kind: 1}) })
	d.Registry().AddGlobalRemoveHandler(c)
	return c, nil
}
func normalize(ds []time.Duration) (map[time.Duration]bool, error) {
	out := map[time.Duration]bool{}
	for _, d := range ds {
		if d == 0 {
			continue
		}
		if d < 0 || d%time.Millisecond != 0 || d/time.Millisecond > math.MaxUint32 {
			return nil, fmt.Errorf("wayland: deadline must be positive whole milliseconds within uint32: %s", d)
		}
		out[d] = true
	}
	return out, nil
}
func (c *Client) HandleRegistryGlobalRemove(wlturbo.RegistryGlobalRemoveEvent) {
	c.enqueue(notice{kind: 1})
}
func (c *Client) enqueue(n notice) {
	select {
	case c.notices <- n:
	default:
		select {
		case c.overflow <- struct{}{}:
		default:
		}
		_ = c.d.Close()
	}
}

// Run uses one dispatch reader; callbacks only enqueue values. All policy,
// notification, output and capability state belongs to the Run goroutine.
func (c *Client) Run(ctx context.Context, commands <-chan ports.WaylandCommand, events chan<- ports.WaylandEvent) error {
	defer c.d.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.d.Close() })
	defer stop()
	if err := c.d.Roundtrip(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	// Bootstrap announcements are represented by the registry snapshot.
	for len(c.notices) > 0 {
		<-c.notices
	}
	if err := c.reconcile(); err != nil {
		return err
	}
	// Initial seat/output metadata and output-power mode arrive before doctor.
	if err := c.d.Roundtrip(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	for len(c.notices) > 0 {
		if err := c.handle(ctx, events, <-c.notices); err != nil {
			return err
		}
	}
	done := make(chan error, 1)
	proceed := make(chan struct{})
	readerStop := make(chan struct{})
	readerExited := make(chan struct{})
	go func() {
		defer close(readerExited)
		for {
			err := c.d.Dispatch()
			select {
			case done <- err:
			case <-readerStop:
				return
			}
			if err != nil {
				return
			}
			select {
			case <-proceed:
			case <-readerStop:
				return
			}
		}
	}()
	defer func() {
		close(readerStop)
		_ = c.d.Close()
		<-readerExited
	}()
	// Callbacks only enqueue immutable proxy references; state stays here.
	var pendingCommands []ports.WaylandCommand
	for {
		if c.dirty {
			if err := c.emit(ctx, events, ports.WaylandCapabilities{Capabilities: c.capabilities()}); err != nil {
				return nil
			}
			c.dirty = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c.overflow:
			return ErrEventOverflow
		case err := <-done:
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				select {
				case <-c.overflow:
					return ErrEventOverflow
				default:
				}
				return err
			}
			for len(c.notices) > 0 {
				if err := c.handle(ctx, events, <-c.notices); err != nil {
					return err
				}
			}
			for _, cmd := range pendingCommands {
				if err := c.command(cmd); err != nil {
					return err
				}
			}
			pendingCommands = nil
			select {
			case proceed <- struct{}{}:
			case <-ctx.Done():
				return nil
			}
		case cmd, ok := <-commands:
			if !ok {
				commands = nil
				continue
			}
			// Process any completed wire frame before this command. This keeps
			// queued old-generation idle and registry removals ahead of reload.
			if len(c.notices) > 0 {
				if len(pendingCommands) == queueSize {
					return ErrEventOverflow
				}
				pendingCommands = append(pendingCommands, cmd)
			} else if err := c.command(cmd); err != nil {
				return err
			}
		}
	}
}
func (c *Client) emit(ctx context.Context, out chan<- ports.WaylandEvent, event ports.WaylandEvent) error {
	select {
	case out <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *Client) handle(ctx context.Context, out chan<- ports.WaylandEvent, n notice) error {
	switch n.kind {
	case 1:
		return c.reconcile()
	case 2, 3:
		p := n.notification
		if !p.active {
			return nil
		}
		if n.kind == 2 {
			if p.idled {
				return nil
			}
			p.idled = true
			if !p.wake {
				return c.emit(ctx, out, ports.WaylandIdle{Generation: p.generation, Proxy: p.token, Cycle: p.cycle, Seat: p.seat, After: p.after})
			}
		}
		if n.kind == 3 {
			if !p.idled {
				return nil
			}
			p.idled = false
			p.cycle++
			if p.wake {
				if err := c.setPower(true); err != nil {
					return err
				}
				return c.emit(ctx, out, ports.WaylandActivity{Proxy: p.token, Cycle: p.cycle, Seat: p.seat})
			}
		}
	case 4:
		o := n.output
		if c.outputs[o.global] != o {
			return nil
		}
		o.name = n.name
		c.dirty = true
	case 6:
		o := n.output
		if c.outputs[o.global] != o || o.failed {
			return nil
		}
		o.mode, o.modeKnown = n.mode, true
		if n.mode == outputpower.MODE_ON {
			o.off = false
		}
	case 5:
		o := n.output
		if c.outputs[o.global] != o || o.failed {
			return nil
		}
		c.log.Warn().Uint64("output_lifetime", o.lifetime).Msg("output power control failed")
		o.failed = true
		o.off = false
		if o.power != nil {
			if err := o.power.Destroy(); err != nil {
				return err
			}
			o.power = nil
		}
		c.dirty = true
		return c.emit(ctx, out, ports.WaylandPowerFailed{Output: o.lifetime})
	}
	return nil
}
func (c *Client) bind(g wlturbo.Global, supported uint32, p wlturbo.Proxy) error {
	return c.d.Registry().Bind(g.Name, g.Interface, min(g.Version, supported), p)
}
func (c *Client) reconcile() error {
	globals := c.d.Registry().GetGlobals()
	find := func(iface string, minVersion uint32) wlturbo.Global {
		var selected wlturbo.Global
		for _, g := range globals {
			if g.Interface == iface && g.Version >= minVersion && (selected.Name == 0 || g.Name < selected.Name) {
				selected = g
			}
		}
		return selected
	}
	idle := find(idlenotify.ExtIdleNotifierInterface, 2)
	wantIdle := c.options.Requirements.Idle || c.options.Requirements.InputWake || len(c.deadlines) > 0
	if !wantIdle {
		idle = wlturbo.Global{}
	}
	if c.idleGlobal != idle.Name {
		for _, s := range c.seats {
			for _, p := range s.notifications {
				if err := c.destroyNotification(p); err != nil {
					return err
				}
			}
			s.notifications = map[time.Duration]*notification{}
			if err := c.destroyNotification(s.wake); err != nil {
				return err
			}
			s.wake = nil
		}
		if c.idle != nil {
			if err := c.idle.Destroy(); err != nil {
				return err
			}
			c.idle = nil
		}
		c.idleGlobal = idle.Name
		if idle.Name != 0 {
			c.idle = idlenotify.NewExtIdleNotifier(c.d.Context())
			if err := c.bind(idle, 2, c.idle); err != nil {
				return err
			}
		}
	}
	power := find(outputpower.OutputPowerManagerInterface, 1)
	if !c.options.Requirements.OutputPower {
		power = wlturbo.Global{}
	}
	if c.powerGlobal != power.Name {
		for _, o := range c.outputs {
			if o.power != nil {
				if o.off {
					if err := o.power.SetMode(outputpower.MODE_ON); err != nil {
						return err
					}
				}
				if err := o.power.Destroy(); err != nil {
					return err
				}
				o.power = nil
			}
			o.off = false
			o.failed = false
		}
		if c.power != nil {
			if err := c.power.Destroy(); err != nil {
				return err
			}
			c.power = nil
		}
		c.powerGlobal = power.Name
		if power.Name != 0 {
			c.power = outputpower.NewOutputPowerManager(c.d.Context())
			if err := c.bind(power, 1, c.power); err != nil {
				return err
			}
		}
	}
	for name, s := range c.seats {
		if g, ok := globals[name]; !ok || g.Interface != core.SeatInterface {
			for _, p := range s.notifications {
				if err := c.destroyNotification(p); err != nil {
					return err
				}
			}
			if err := c.destroyNotification(s.wake); err != nil {
				return err
			}
			if s.proxy.Version() >= 5 {
				if err := s.proxy.Release(); err != nil {
					return err
				}
			}
			delete(c.seats, name)
		}
	}
	for name, o := range c.outputs {
		if g, ok := globals[name]; !ok || g.Interface != core.OutputInterface {
			if o.power != nil {
				if err := o.power.Destroy(); err != nil {
					return err
				}
			}
			if o.proxy.Version() >= 3 {
				if err := o.proxy.Release(); err != nil {
					return err
				}
			}
			delete(c.outputs, name)
		}
	}
	for name, g := range globals {
		if g.Interface == core.SeatInterface && c.seats[name] == nil {
			p := core.NewSeat(c.d.Context())
			if err := c.bind(g, 5, p); err != nil {
				return err
			}
			c.seats[name] = &seat{proxy: p, notifications: map[time.Duration]*notification{}}
		}
		if g.Interface == core.OutputInterface && c.outputs[name] == nil {
			c.next++
			o := &output{global: name, lifetime: c.next, proxy: core.NewOutput(c.d.Context())}
			o.proxy.OnName(func(s string) { c.enqueue(notice{kind: 4, output: o, name: s}) })
			if err := c.bind(g, 4, o.proxy); err != nil {
				return err
			}
			c.outputs[name] = o
		}
	}
	for name, s := range c.seats {
		for d, p := range s.notifications {
			if !c.deadlines[d] {
				if err := c.destroyNotification(p); err != nil {
					return err
				}
				delete(s.notifications, d)
			} else {
				p.generation = c.generation
			}
		}
		if c.idle != nil {
			for d := range c.deadlines {
				if s.notifications[d] == nil {
					p, err := c.newNotification(name, s, d, false)
					if err != nil {
						return err
					}
					s.notifications[d] = p
				}
			}
			if c.options.Requirements.InputWake && s.wake == nil {
				p, err := c.newNotification(name, s, 0, true)
				if err != nil {
					return err
				}
				s.wake = p
			}
		}
	}
	for _, o := range c.outputs {
		if c.power != nil && o.power == nil && !o.failed {
			p, err := c.power.GetOutputPower(o.proxy)
			if err != nil {
				return err
			}
			o.power = p
			p.OnMode(func(mode uint32) { c.enqueue(notice{kind: 6, output: o, mode: mode}) })
			p.OnFailed(func() { c.enqueue(notice{kind: 5, output: o}) })
		}
	}
	c.dirty = true
	return nil
}
func (c *Client) newNotification(name uint32, s *seat, d time.Duration, wake bool) (*notification, error) {
	var proxy *idlenotify.ExtIdleNotification
	var err error
	if wake {
		proxy, err = c.idle.GetInputIdleNotification(0, s.proxy)
	} else {
		proxy, err = c.idle.GetIdleNotification(uint32(d/time.Millisecond), s.proxy)
	}
	if err != nil {
		return nil, err
	}
	c.next++
	p := &notification{proxy: proxy, token: c.next, cycle: 1, generation: c.generation, seat: name, after: d, active: true, wake: wake}
	proxy.OnIdled(func() { c.enqueue(notice{kind: 2, notification: p}) })
	proxy.OnResumed(func() { c.enqueue(notice{kind: 3, notification: p}) })
	return p, nil
}
func (c *Client) destroyNotification(p *notification) error {
	if p == nil {
		return nil
	}
	p.active = false
	return p.proxy.Destroy()
}
func (c *Client) setPower(on bool) error {
	for _, o := range c.outputs {
		if o.power == nil || o.failed {
			continue
		}
		if on && !o.off {
			continue
		}
		if !on && (o.off || o.modeKnown && o.mode == outputpower.MODE_OFF) {
			continue // never claim an output somebody else already turned off
		}
		mode := uint32(outputpower.MODE_OFF)
		if on {
			mode = outputpower.MODE_ON
		}
		if err := o.power.SetMode(mode); err != nil {
			return err
		}
		o.off = !on
	}
	return nil
}
func (c *Client) capabilities() ports.Capabilities {
	result := ports.Capabilities{Globals: map[string]uint32{}}
	for _, g := range c.d.Registry().GetGlobals() {
		result.Globals[g.Interface] = max(result.Globals[g.Interface], g.Version)
	}
	for name := range c.seats {
		result.Seats = append(result.Seats, name)
	}
	sort.Slice(result.Seats, func(i, j int) bool { return result.Seats[i] < result.Seats[j] })
	for _, o := range c.outputs {
		result.Outputs = append(result.Outputs, ports.WaylandOutput{Lifetime: o.lifetime, Global: o.global, Name: o.name, PowerSupported: o.power != nil && !o.failed})
	}
	sort.Slice(result.Outputs, func(i, j int) bool { return result.Outputs[i].Global < result.Outputs[j].Global })
	require := func(enabled bool, name string, version uint32) {
		if enabled && result.Globals[name] < version {
			result.Missing = append(result.Missing, fmt.Sprintf("%s >= %d", name, version))
		}
	}
	r := c.options.Requirements
	require(r.Idle || r.InputWake || len(c.deadlines) > 0, idlenotify.ExtIdleNotifierInterface, 2)
	require(r.Idle || r.InputWake || len(c.deadlines) > 0, core.SeatInterface, 1)
	require(r.OutputPower, outputpower.OutputPowerManagerInterface, 1)
	require(r.OutputPower || r.Visual || r.Lock, core.OutputInterface, 1)
	if r.OutputPower {
		for _, o := range c.outputs {
			if o.failed {
				result.Missing = append(result.Missing, fmt.Sprintf("%s output lifetime %d", outputpower.OutputPowerInterface, o.lifetime))
			}
		}
	}
	require(r.Lock, "ext_session_lock_manager_v1", 1)
	require(r.Visual, "wl_compositor", 1)
	require(r.Visual, "zwp_linux_dmabuf_v1", 4)
	require(r.Visual, "wp_linux_drm_syncobj_manager_v1", 1)
	sort.Strings(result.Missing)
	return result
}

func (c *Client) command(cmd ports.WaylandCommand) error {
	switch v := cmd.(type) {
	case ports.SetDeadlines:
		ds, err := normalize(v.Deadlines)
		if err != nil {
			return err
		}
		c.deadlines, c.generation = ds, v.Generation
		return c.reconcile()
	case ports.OutputPower:
		return c.setPower(v.On)
	}
	return nil
}

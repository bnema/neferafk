// Package app wires adapters to the pure policy. The Owner goroutine is the
// only place policy state is read or written; adapters talk to it through
// channels and the ports.Visual port. Nothing here can unlock a session:
// unlock exists only as a visual-reported LockReleased lifecycle result.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/bnema/neferafk/internal/core"
	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/zerowrap"
)

const maxQueue = 256

// Inputs are the owner's event channels. Any may be nil (feature absent).
type Inputs struct {
	Config   <-chan ports.ConfigChanged
	Failures <-chan error
	Wayland  <-chan ports.WaylandEvent
	System   <-chan ports.SystemEvent
	Visual   <-chan ports.VisualEvent
	Control  <-chan ports.ControlRequest
}

// Outputs are adapter command channels. The owner never blocks on them: sends
// go through bounded per-channel queues so an adapter that is itself blocked
// emitting an event cannot deadlock with the owner.
type Outputs struct {
	Wayland chan<- ports.WaylandCommand
	System  chan<- ports.SystemCommand
}

type Options struct {
	Config ports.Config
	Visual ports.Visual
	// LookupEnv reads the daemon's own startup environment for the PIN env
	// source (the accepted, documented residual custody). Defaults to os.LookupEnv.
	LookupEnv func(string) (string, bool)
	Log       zerowrap.Logger
	// RetryMin/RetryMax bound the lock (re)spawn backoff; defaults 250ms/5s.
	RetryMin, RetryMax time.Duration
	// Gate, when set, makes Run fail with *UnsupportedError unless the latest
	// Wayland report and the first System report (if ExpectSystem) are
	// complete within GateTimeout (default 10s). Missing System support fails
	// at once; missing Wayland globals may still be advertised late.
	Gate         bool
	ExpectSystem bool
	GateTimeout  time.Duration
}

type Owner struct {
	policy    *core.Policy
	cfg       ports.Config
	startWL   ports.WaylandRequirements
	startSY   ports.SystemRequirements
	startAuth ports.AuthConfig
	opt       Options
	in        Inputs
	out       Outputs
	log       zerowrap.Logger
	initial   []ports.PolicyEffect

	wq         []ports.WaylandCommand
	sq         []ports.SystemCommand
	regs       []ports.IdleNotification
	regGen     map[uint64]ports.Generation
	regOrigin  map[uint64]time.Time              // when policy first registered each ID
	idle       map[uint32]map[time.Duration]bool // adapter proxies currently idle, per seat and duration
	seats      map[uint32]bool                   // seats from the latest capability report
	seatsKnown bool
	fired      map[uint64]bool // registrations already delivered to policy
	deferred   *time.Timer     // fires registrations whose origin+After lies ahead
	lockGen    ports.Generation
	delay      time.Duration
	retry      *time.Timer
	sleeping   bool
	sleepCycle uint64
	wlCaps     *ports.Capabilities
	sysCaps    *ports.SystemCapabilities
	gated      bool
}

func NewOwner(o Options, in Inputs, out Outputs) (*Owner, error) {
	if o.Visual == nil {
		return nil, errors.New("app: visual port required")
	}
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	if o.RetryMin <= 0 {
		o.RetryMin = 250 * time.Millisecond
	}
	if o.RetryMax < o.RetryMin {
		o.RetryMax = max(5*time.Second, o.RetryMin)
	}
	if o.GateTimeout <= 0 {
		o.GateTimeout = 10 * time.Second
	}
	p, initial, err := core.New(o.Config, time.Now())
	if err != nil {
		return nil, err
	}
	w, s := Requirements(o.Config)
	ow := &Owner{policy: p, cfg: o.Config, startAuth: o.Config.Auth, startWL: w, startSY: s, opt: o, in: in, out: out, log: o.Log.WithField("component", "app"), initial: initial, regGen: map[uint64]ports.Generation{}, regOrigin: map[uint64]time.Time{}, idle: map[uint32]map[time.Duration]bool{}, seats: map[uint32]bool{}, fired: map[uint64]bool{}, delay: o.RetryMin}
	ow.retry = time.NewTimer(time.Hour)
	ow.retry.Stop()
	ow.deferred = time.NewTimer(time.Hour)
	ow.deferred.Stop()
	ow.gated = !o.Gate
	return ow, nil
}

// Run is the policy owner goroutine. It returns nil on ctx cancellation, an
// *UnsupportedError when the startup gate fails, or a fatal channel error.
func (o *Owner) Run(ctx context.Context) error {
	defer o.retry.Stop()
	defer o.deferred.Stop()
	defer o.shutdown()
	for _, e := range o.initial {
		if err := o.apply(ctx, e); err != nil {
			return err
		}
	}
	o.initial = nil
	var gate <-chan time.Time
	var gateTimer *time.Timer
	if o.opt.Gate && !o.gated {
		gateTimer = time.NewTimer(o.opt.GateTimeout)
		defer gateTimer.Stop()
		gate = gateTimer.C
	}
	for {
		var wc chan<- ports.WaylandCommand
		var wv ports.WaylandCommand
		if len(o.wq) > 0 {
			wc, wv = o.out.Wayland, o.wq[0]
		}
		var sc chan<- ports.SystemCommand
		var sv ports.SystemCommand
		if len(o.sq) > 0 {
			sc, sv = o.out.System, o.sq[0]
		}
		var err error
		select {
		case <-ctx.Done():
			return nil
		case <-gate:
			if o.wlCaps != nil && (o.sysCaps != nil || !o.opt.ExpectSystem) {
				return &UnsupportedError{Missing: unsupported(o.wlCaps, o.sysCaps)}
			}
			return &UnsupportedError{Missing: []string{"capability reports not received before timeout"}}
		case wc <- wv:
			o.wq = o.wq[1:]
		case sc <- sv:
			o.sq = o.sq[1:]
		case ev, ok := <-o.in.Config:
			if !ok {
				return errors.New("config channel closed")
			}
			err = o.reload(ctx, ev.Config)
		case e, ok := <-o.in.Failures:
			if !ok {
				o.in.Failures = nil
				continue
			}
			o.log.Warn().Err(e).Msg("config change rejected; keeping last valid configuration")
		case ev, ok := <-o.in.Wayland:
			if !ok {
				return errors.New("wayland channel closed")
			}
			err = o.wayland(ctx, ev)
		case ev, ok := <-o.in.System:
			if !ok {
				return errors.New("system channel closed")
			}
			err = o.system(ctx, ev)
		case ev, ok := <-o.in.Visual:
			if !ok {
				return errors.New("visual channel closed")
			}
			err = o.visual(ctx, ev)
		case req, ok := <-o.in.Control:
			if !ok {
				o.in.Control = nil
				continue
			}
			err = o.control(ctx, req)
		case <-o.retry.C:
			err = o.startLock(ctx)
		case <-o.deferred.C:
			err = o.fireDue(ctx)
		}
		if err != nil {
			return err
		}
		if e := o.checkGate(); e != nil {
			return e
		}
		if o.gated && gateTimer != nil {
			gateTimer.Stop() // gate passed: the timeout must never fire afterwards
			gateTimer, gate = nil, nil
		}
		if len(o.wq) > maxQueue || len(o.sq) > maxQueue {
			return errors.New("adapter command queue overflow")
		}
	}
}

func (o *Owner) checkGate() error {
	if o.gated || o.wlCaps == nil || o.opt.ExpectSystem && o.sysCaps == nil {
		return nil
	}
	if m := unsupported(o.wlCaps, o.sysCaps); len(m) > 0 {
		if len(o.wlCaps.Missing) > 0 && len(unsupported(nil, o.sysCaps)) == 0 {
			return nil // wait for a later Wayland report or the gate timeout
		}
		o.gated = true
		return &UnsupportedError{Missing: m}
	}
	o.gated = true
	if w := suspendWarning(o.cfg, o.sysCaps); w != "" {
		o.log.Warn().Msg(w)
	}
	return nil
}

// shutdown runs on every Run exit. A held (or being acquired) lock is never
// torn down: the visual is detached and keeps its lock and auth UI.
func (o *Owner) shutdown() {
	st := o.policy.State()
	var err error
	if st.Acquiring || st.Protected {
		// Never tear down a lock we hold: leave the visual (and its auth UI) up.
		err = o.opt.Visual.Detach()
	} else {
		err = o.opt.Visual.Close()
	}
	if err != nil {
		o.log.Warn().Err(err).Msg("visual shutdown")
	}
}

// handle feeds the policy and applies the resulting effects in order.
func (o *Owner) handle(ctx context.Context, ev ports.PolicyEvent) error {
	effects, err := o.policy.Handle(time.Now(), ev)
	if err != nil {
		o.log.Warn().Err(err).Msgf("policy rejected %T", ev)
		return nil
	}
	for _, e := range effects {
		if err := o.apply(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (o *Owner) apply(ctx context.Context, effect ports.PolicyEffect) error {
	switch e := effect.(type) {
	case ports.ArmIdle:
		regs := map[uint64]ports.Generation{}
		origins := map[uint64]time.Time{}
		var ds []time.Duration
		now := time.Now()
		for _, n := range e.Notifications {
			g, ok := o.regGen[n.ID]
			if !ok {
				g = e.Generation // retained registrations keep their admitting generation
			}
			regs[n.ID] = g
			origins[n.ID] = now
			if t, ok := o.regOrigin[n.ID]; ok {
				origins[n.ID] = t
			}
			if !slices.Contains(ds, n.After) {
				ds = append(ds, n.After)
			}
		}
		fired := map[uint64]bool{}
		for id := range regs {
			fired[id] = o.fired[id]
		}
		o.regGen, o.regOrigin, o.regs, o.fired = regs, origins, e.Notifications, fired
		for _, m := range o.idle {
			for d := range m {
				if !slices.Contains(ds, d) {
					delete(m, d) // adapter destroys that proxy; a re-add starts fresh
				}
			}
		}
		o.armDeferred()
		// Always sent, even when deadlines are unchanged: the adapter stamps
		// idle events with the newest generation it was told about, and it
		// retains proxies (no rebase) when durations are unchanged.
		o.wq = append(o.wq, ports.SetDeadlines{Generation: uint64(e.Generation), Deadlines: ds})
	case ports.AcquireLock:
		o.lockGen = e.Generation
		o.delay = o.opt.RetryMin
		return o.startLock(ctx)
	case ports.SetFade:
		if err := o.opt.Visual.Fade(ctx, ports.VisualFade{Black: e.Black, Duration: e.Duration, Spawn: o.spawn()}); err != nil {
			o.log.Warn().Err(err).Bool("black", e.Black).Msg("fade request failed")
		}
	case ports.SetOutputPower:
		o.wq = append(o.wq, ports.OutputPower(e))
	case ports.Suspend:
		o.sq = append(o.sq, ports.SystemSuspend{})
	case ports.SleepReady:
		if o.sleeping {
			o.sq = append(o.sq, ports.SystemSleepReady{Cycle: o.sleepCycle})
		}
	}
	return nil
}

// spawn omits both the current and the startup-captured pin-env names: the
// startup value is what the daemon holds, whatever a reload later selects.
func (o *Owner) spawn() ports.VisualSpawn {
	var names []string
	for _, a := range []ports.AuthConfig{o.cfg.Auth, o.startAuth} {
		if a.PINEnv != "" && !slices.Contains(names, a.PINEnv) {
			names = append(names, a.PINEnv)
		}
	}
	return ports.VisualSpawn{OmitEnv: names}
}

func (o *Owner) bootstrap() ports.AuthBootstrap {
	a := o.cfg.Auth
	b := ports.AuthBootstrap{Generation: uint64(o.lockGen), PINSource: a.PINSource}
	switch a.PINSource {
	case ports.PINSourceEnv:
		if v, ok := o.opt.LookupEnv(a.PINEnv); ok {
			b.EnvPIN = []byte(v)
		}
	case ports.PINSourcePass:
		b.PINReference = a.PINEntry
	}
	return b
}

func (o *Owner) lockWanted() bool {
	st := o.policy.State()
	return st.Acquiring || st.Protected
}

// startLock asks the visual to hold ext-session-lock for the lock generation,
// (re)spawning it as needed. Failures back off and retry; the compositor keeps
// any acquired lock in the meantime.
func (o *Owner) startLock(ctx context.Context) error {
	if !o.lockWanted() || o.lockGen == 0 {
		return nil
	}
	err := o.opt.Visual.Lock(ctx, ports.VisualLock{Generation: o.lockGen, Auth: o.bootstrap(), Spawn: o.spawn()})
	if err != nil {
		o.log.Warn().Err(err).Uint64("generation", uint64(o.lockGen)).Dur("retry_in", o.delay).Msg("lock visual unavailable")
		o.armRetry()
	}
	return nil
}

func (o *Owner) armRetry() {
	o.retry.Reset(o.delay)
	o.delay = min(o.delay*2, o.opt.RetryMax)
}

func (o *Owner) reload(ctx context.Context, cfg ports.Config) error {
	if err := cfg.Validate(); err != nil {
		o.log.Warn().Err(err).Msg("config reload rejected")
		return nil
	}
	w, s := Requirements(cfg)
	if w.OutputPower && !o.startWL.OutputPower || s.Sleep && !o.startSY.Sleep {
		o.log.Warn().Msg("reload enables output-off or sleep support that was not initialised at startup; restart the daemon")
	}
	if o.gated && o.cfg.SleepAfter == 0 {
		if w := suspendWarning(cfg, o.sysCaps); w != "" {
			o.log.Warn().Msg(w)
		}
	}
	o.cfg = cfg
	return o.handle(ctx, ports.Reload{Config: cfg})
}

func (o *Owner) wayland(ctx context.Context, ev ports.WaylandEvent) error {
	switch v := ev.(type) {
	case ports.WaylandIdle:
		// The adapter keeps one proxy per distinct duration, measured from the
		// last input, while policy registrations (one per origin) are relative
		// to their own registration time. A registration is delivered once its
		// duration is idle and its origin+After has passed; later ones wait on
		// the deferred timer (this is what keeps an equal-valued reload from
		// rebasing early). Policy drops anything stale.
		if o.idle[v.Seat] == nil {
			o.idle[v.Seat] = map[time.Duration]bool{}
		}
		o.idle[v.Seat][v.After] = true
		return o.fireDue(ctx)
	case ports.WaylandActivity:
		// Undoes fade and wakes outputs; the policy has no unlock path.
		// Only the active seat stops being idle; other seats keep their state.
		delete(o.idle, v.Seat)
		clear(o.fired)
		return o.handle(ctx, ports.Activity{})
	case ports.WaylandCapabilities:
		c := v.Capabilities
		if o.gated && o.wlCaps != nil && len(c.Missing) > 0 {
			o.log.Warn().Strs("missing", c.Missing).Msg("wayland capabilities degraded")
		}
		if !o.gated || o.wlCaps == nil {
			// Until the gate passes the latest report counts: a compositor may
			// advertise outputs after the daemon connected.
			o.wlCaps = &c
		}
		o.seats, o.seatsKnown = map[uint32]bool{}, true
		for _, id := range c.Seats {
			o.seats[id] = true
		}
		for id := range o.idle {
			if !o.seats[id] {
				delete(o.idle, id) // removed seat no longer counts
			}
		}
		return o.fireDue(ctx)
	case ports.WaylandPowerFailed:
		o.log.Warn().Uint64("output", v.Output).Msg("output power control failed")
	}
	return nil
}

// allIdle reports whether every known seat has duration d idle. Known seats are
// the latest capability report's; nothing fires before the first report.
func (o *Owner) allIdle(d time.Duration) bool {
	if !o.seatsKnown || len(o.seats) == 0 {
		return false
	}
	for id := range o.seats {
		if !o.idle[id][d] {
			return false
		}
	}
	return true
}

// fireDue delivers every registration whose duration is idle on all known
// seats and whose origin+After has passed, and re-arms the deferred timer.
func (o *Owner) fireDue(ctx context.Context) error {
	now := time.Now()
	for _, n := range slices.Clone(o.regs) {
		if !o.allIdle(n.After) || o.fired[n.ID] || o.regOrigin[n.ID].Add(n.After).After(now) {
			continue
		}
		o.fired[n.ID] = true
		if err := o.handle(ctx, ports.IdleDue{Generation: o.regGen[n.ID], Notification: n.ID, After: n.After}); err != nil {
			return err
		}
	}
	o.armDeferred()
	return nil
}

func (o *Owner) armDeferred() {
	o.deferred.Stop()
	var next time.Time
	for _, n := range o.regs {
		if !o.allIdle(n.After) || o.fired[n.ID] {
			continue
		}
		if t := o.regOrigin[n.ID].Add(n.After); next.IsZero() || t.Before(next) {
			next = t
		}
	}
	if !next.IsZero() {
		o.deferred.Reset(max(time.Until(next), time.Millisecond))
	}
}

func (o *Owner) system(ctx context.Context, ev ports.SystemEvent) error {
	switch v := ev.(type) {
	case ports.SystemCapabilityReport:
		c := v.Capabilities
		if o.sysCaps == nil {
			o.sysCaps = &c
		}
	case ports.SystemLockRequested:
		return o.handle(ctx, ports.LockRequested{})
	case ports.SystemUnlockObserved:
		// An observation, never authority: only the visual releases the lock.
		o.log.Info().Msg("logind unlock observed; ignored")
	case ports.SleepPreparation:
		o.sleeping, o.sleepCycle = v.Preparing, v.Cycle
		if v.Preparing {
			return o.handle(ctx, ports.BeforeSleep{})
		}
	case ports.SystemFailure:
		o.log.Warn().Err(v.Err).Str("operation", v.Operation).Msg("system operation failed")
	}
	return nil
}

func (o *Owner) visual(ctx context.Context, ev ports.VisualEvent) error {
	switch v := ev.(type) {
	case ports.LockConfirmed:
		o.delay = o.opt.RetryMin
		return o.handle(ctx, v)
	case ports.LockReleased:
		if err := o.handle(ctx, v); err != nil {
			return err
		}
		if !o.lockWanted() {
			o.lockGen = 0
			o.retry.Stop()
		}
	case ports.VisualExited:
		if o.lockWanted() {
			// A dead lock visual leaves the session locked; bring it back.
			o.log.Warn().Err(v.Err).Uint64("generation", uint64(o.lockGen)).Msg("lock visual exited; respawning")
			o.armRetry()
		} else {
			o.log.Info().Err(v.Err).Msg("visual exited")
		}
	}
	return nil
}

func (o *Owner) control(ctx context.Context, req ports.ControlRequest) error {
	if req.Reply == nil {
		return nil // nobody to answer, and nothing to apply
	}
	if req.Kind == ports.ControlLock {
		if err := o.handle(ctx, ports.LockRequested{}); err != nil {
			return err
		}
	} else if req.Kind != ports.ControlStatus {
		req.Reply <- ports.ControlReply{Err: fmt.Sprintf("unknown request %d", req.Kind)}
		return nil
	}
	st := o.policy.State()
	req.Reply <- ports.ControlReply{Generation: st.Generation, Acquiring: st.Acquiring, Protected: st.Protected, Faded: st.Faded, Off: st.Off}
	return nil
}

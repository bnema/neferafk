// Package core owns pure absence policy. Call Handle on one owner goroutine;
// adapters perform returned effects. Times are explicit, not inferred idle age.
package core

import (
	"fmt"
	"slices"
	"time"

	"github.com/bnema/neferafk/internal/ports"
)

type deadline struct {
	after        time.Duration
	notification uint64
	done, due    bool
}
type registration struct {
	id         uint64
	generation ports.Generation
	after      time.Duration
	actions    []ports.Action
}
type Policy struct {
	cfg                     ports.Config
	generation              ports.Generation
	nextNotification        uint64
	deadlines               [4]deadline
	registrations           []registration
	acquiring, protected    bool
	lockGeneration          ports.Generation
	faded, off, beforeSleep bool
}
type State struct {
	Generation                       ports.Generation
	Acquiring, Protected, Faded, Off bool
}

func (p *Policy) State() State { return State{p.generation, p.acquiring, p.protected, p.faded, p.off} }
func afters(c ports.Config) [4]time.Duration {
	return [4]time.Duration{c.LockAfter, c.FadeAfter, c.OffAfter, c.SleepAfter}
}

func New(cfg ports.Config, now time.Time) (*Policy, []ports.PolicyEffect, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	p := &Policy{cfg: cfg, generation: 1}
	for a, d := range afters(cfg) {
		p.deadlines[a] = deadline{after: d}
	}
	return p, p.arm(now), nil
}

func (p *Policy) arm(_ time.Time) []ports.PolicyEffect {
	old := p.registrations
	p.registrations = nil
	for a := range p.deadlines {
		d := &p.deadlines[a]
		if d.after == 0 || d.done || d.due {
			continue
		}
		i := slices.IndexFunc(p.registrations, func(r registration) bool {
			return d.notification != 0 && r.id == d.notification || d.notification == 0 && r.generation == p.generation && r.after == d.after
		})
		if i < 0 {
			r := registration{after: d.after, generation: p.generation}
			if d.notification != 0 {
				j := slices.IndexFunc(old, func(r registration) bool { return r.id == d.notification })
				if j >= 0 {
					r = old[j]
					r.actions = nil
				}
			}
			if r.id == 0 {
				p.nextNotification++
				r.id = p.nextNotification
			}
			r.actions = []ports.Action{ports.Action(a)}
			p.registrations = append(p.registrations, r)
			d.notification = r.id
		} else {
			p.registrations[i].actions = append(p.registrations[i].actions, ports.Action(a))
			d.notification = p.registrations[i].id
		}
	}
	slices.SortFunc(p.registrations, func(a, b registration) int {
		if a.after < b.after {
			return -1
		}
		if a.after > b.after {
			return 1
		}
		return 0
	})
	n := make([]ports.IdleNotification, 0, len(p.registrations))
	for _, r := range p.registrations {
		n = append(n, ports.IdleNotification{ID: r.id, After: r.after})
	}
	return []ports.PolicyEffect{ports.ArmIdle{Generation: p.generation, Notifications: n}}
}

func (p *Policy) acquire(out *[]ports.PolicyEffect) {
	if p.acquiring || p.protected {
		return
	}
	p.acquiring = true
	p.lockGeneration = p.generation
	*out = append(*out, ports.AcquireLock{Generation: p.lockGeneration})
}

func (p *Policy) dispatch() []ports.PolicyEffect {
	var out []ports.PolicyEffect
	// Due flags for one notification are established before dispatch; lock is
	// first only for simultaneous due actions, never because enabled/later.
	for a := ports.Lock; a <= ports.Sleep; a++ {
		d := &p.deadlines[a]
		if !d.due || d.done || d.after == 0 {
			continue
		}
		if (a == ports.OutputOff || a == ports.Sleep) && p.acquiring {
			continue
		}
		d.done = true
		switch a {
		case ports.Lock:
			p.acquire(&out)
		case ports.Fade:
			p.faded = true
			out = append(out, ports.SetFade{Black: true, Duration: p.cfg.FadeDuration})
		case ports.OutputOff:
			p.off = true
			out = append(out, ports.SetOutputPower{On: false})
		case ports.Sleep:
			out = append(out, ports.Suspend{})
		}
	}
	return out
}

func (p *Policy) Handle(now time.Time, event ports.PolicyEvent) ([]ports.PolicyEffect, error) {
	switch v := event.(type) {
	case ports.IdleDue:
		i := slices.IndexFunc(p.registrations, func(r registration) bool {
			return r.generation == v.Generation && (v.Notification != 0 && r.id == v.Notification || v.Notification == 0 && r.after == v.After)
		})
		if i < 0 {
			return nil, nil
		}
		for _, a := range p.registrations[i].actions {
			p.deadlines[a].due = true
		}
		return p.dispatch(), nil
	case ports.Activity:
		p.generation++
		var out []ports.PolicyEffect
		if p.faded {
			p.faded = false
			out = append(out, ports.SetFade{Black: false, Duration: p.cfg.FadeDuration})
		}
		if p.off {
			p.off = false
			out = append(out, ports.SetOutputPower{On: true})
		}
		for a, d := range afters(p.cfg) {
			p.deadlines[a] = deadline{after: d}
		}
		return append(out, p.arm(now)...), nil
	case ports.Reload:
		if err := v.Config.Validate(); err != nil {
			return nil, err
		}
		if v.Config == p.cfg {
			return nil, nil
		}
		for a, d := range afters(v.Config) {
			old := &p.deadlines[a]
			if old.after != d {
				// Enablement changes even for completed actions; completion is
				// pinned for this cycle, but old due/proxy identity is retired.
				*old = deadline{after: d, done: old.done}
			}
		}
		p.cfg = v.Config
		p.generation++
		return p.arm(now), nil
	case ports.LockConfirmed:
		if !p.acquiring || v.Generation != p.lockGeneration {
			return nil, nil
		}
		p.acquiring = false
		p.protected = true
		out := p.dispatch()
		if p.beforeSleep {
			p.beforeSleep = false
			out = append(out, ports.SleepReady{})
		}
		return out, nil
	case ports.LockReleased:
		if !p.protected || v.Generation != p.lockGeneration {
			return nil, nil
		}
		p.protected = false // only authoritative successful visual-owner release
		return nil, nil
	case ports.LockRequested:
		var out []ports.PolicyEffect
		p.acquire(&out)
		return out, nil
	case ports.BeforeSleep:
		if p.protected {
			return []ports.PolicyEffect{ports.SleepReady{}}, nil
		}
		if p.cfg.LockAfter == 0 && !p.acquiring {
			return []ports.PolicyEffect{ports.SleepReady{}}, nil
		}
		p.beforeSleep = true
		var out []ports.PolicyEffect
		p.acquire(&out)
		return out, nil
	default:
		return nil, fmt.Errorf("unknown policy event %T", event)
	}
}

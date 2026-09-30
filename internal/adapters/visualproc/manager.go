package visualproc

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/zerowrap"
)

type live struct {
	session ports.VisualSession
	exited  atomic.Bool
	lockGen atomic.Uint64 // generation the visual was last asked to lock; cleared on release
}

// Manager implements ports.Visual over a VisualLauncher. Fade/Lock/Close/Detach
// are called from the policy owner goroutine only; forwarding goroutines touch
// just the atomic exited flag and the event channel. The visual is spawned
// lazily and never asked to unlock: there is no such command.
type Manager struct {
	ctx      context.Context
	cancel   context.CancelFunc
	launcher ports.VisualLauncher
	out      chan<- ports.VisualEvent
	log      zerowrap.Logger
	cur      *live
}

func NewManager(ctx context.Context, launcher ports.VisualLauncher, out chan<- ports.VisualEvent, log zerowrap.Logger) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	return &Manager{ctx: ctx, cancel: cancel, launcher: launcher, out: out, log: log.WithField("component", "visual")}
}

// current returns the running session or launches one when spawn is true.
func (m *Manager) current(ctx context.Context, spawn bool, sp ports.VisualSpawn) (*live, error) {
	if m.cur != nil && m.cur.exited.Load() {
		if err := m.cur.session.Close(); err != nil {
			m.log.Warn().Err(err).Msg("closing exited visual session")
		}
		m.cur = nil
	}
	if m.cur != nil || !spawn {
		return m.cur, nil
	}
	s, err := m.launcher.Launch(ctx, sp)
	if err != nil {
		return nil, err
	}
	l := &live{session: s}
	m.cur = l
	go m.forward(l)
	m.log.Info().Msg("visual process spawned")
	return l, nil
}

// forward relays child events. The exited flag is set before the terminal
// event is delivered, so a handler reacting to VisualExited already observes it.
func (m *Manager) forward(l *live) {
	for ev := range l.session.Events() {
		switch v := ev.(type) {
		case ports.VisualExited:
			l.exited.Store(true)
		case ports.LockReleased:
			l.lockGen.CompareAndSwap(uint64(v.Generation), 0)
		}
		select {
		case m.out <- ev:
		case <-m.ctx.Done():
			return
		}
	}
	l.exited.Store(true)
}

// send retries once on a fresh process when the current one died between the
// liveness check and the write.
func (m *Manager) send(ctx context.Context, spawn bool, sp ports.VisualSpawn, cmd ports.VisualCommand, mark func(*live)) error {
	for attempt := 0; attempt < 2; attempt++ {
		l, err := m.current(ctx, spawn, sp)
		if err != nil || l == nil {
			return err
		}
		if err = l.session.Send(cmd); err == nil {
			if mark != nil {
				mark(l)
			}
			return nil
		}
		l.exited.Store(true)
	}
	return errors.New("visual unreachable")
}

func (m *Manager) Fade(ctx context.Context, req ports.VisualFade) error {
	// Reveal never spawns a process.
	return m.send(ctx, req.Black, req.Spawn, req, nil)
}

func (m *Manager) Lock(ctx context.Context, req ports.VisualLock) error {
	defer clear(req.Auth.EnvPIN)
	if m.cur != nil && !m.cur.exited.Load() && m.cur.lockGen.Load() == uint64(req.Generation) {
		return nil
	}
	return m.send(ctx, true, req.Spawn, req, func(l *live) { l.lockGen.Store(uint64(req.Generation)) })
}

func (m *Manager) Close() error {
	defer m.cancel()
	if m.cur == nil {
		return nil
	}
	err := m.cur.session.Close()
	m.cur = nil
	return err
}

func (m *Manager) Detach() error {
	defer m.cancel()
	if m.cur == nil {
		return nil
	}
	err := m.cur.session.Detach()
	m.cur = nil
	return err
}

var _ ports.Visual = (*Manager)(nil)

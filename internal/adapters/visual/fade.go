package visual

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/bnema/neferclient"
	"github.com/bnema/nefergui"
)

// fadeTick is the redraw cadence while the fade animates.
const fadeTick = 16 * time.Millisecond

// fadeCmd retargets the fade: black fades to opaque, otherwise back to clear.
type fadeCmd struct {
	black bool
	dur   time.Duration
}

// fadeModel is the overlay's animation state, shared by the surfaces of every
// output. The view (NeferGUI owner loop) owns the fields below "owned"; other
// goroutines only use send and animatingAt.
type fadeModel struct {
	cmds chan fadeCmd
	wake chan struct{}
	// base is the monotonic origin for tickUntil; fixed at construction.
	base time.Time
	// tickUntil keeps redraws coming past the fade end, as nanoseconds since
	// base (monotonic, like fadeAlpha). It must not depend on which surface
	// drew last: a wake redraws all surfaces, but each draws at its own instant.
	tickUntil atomic.Int64
	// finish ends the run once a reveal completed; set by the runner before
	// the loop starts.
	finish context.CancelFunc

	// owned by the view:
	started  bool
	from, to float64
	start    time.Time
	dur      time.Duration
}

func newFadeModel() *fadeModel {
	return &fadeModel{cmds: make(chan fadeCmd, 4), wake: make(chan struct{}, 1), base: time.Now()}
}

// send queues a retarget (dropping the oldest when full) and requests a redraw.
func (m *fadeModel) send(c fadeCmd) {
	for {
		select {
		case m.cmds <- c:
			m.poke()
			return
		default:
			select {
			case <-m.cmds:
			default:
			}
		}
	}
}

func (m *fadeModel) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// fadeAlpha is the overlay opacity at now and whether the fade has arrived.
func fadeAlpha(from, to float64, start time.Time, dur time.Duration, now time.Time) (float64, bool) {
	if dur <= 0 || !now.Before(start.Add(dur)) {
		return to, true
	}
	t := float64(now.Sub(start)) / float64(dur)
	return from + (to-from)*t, false
}

// fadeCSS is the full-screen black layer at opacity a.
func fadeCSS(a float64) string {
	return fmt.Sprintf("width:100%%;height:100%%;background-color:rgb(0 0 0 / %.1f%%)", a*100)
}

// animatingAt reports whether the ticker must still request redraws.
func (m *fadeModel) animatingAt(now time.Time) bool {
	return int64(now.Sub(m.base)) < m.tickUntil.Load()
}

// fadeView applies queued retargets, then draws the overlay for this instant.
func fadeView(f *nefergui.Frame, m *fadeModel) {
	a, done := m.step(time.Now())
	f.Root(nefergui.Inline(fadeCSS(a)))
	if done && m.started && m.to == 0 && m.finish != nil {
		m.finish()
	}
}

// step applies queued retargets and returns the opacity at now.
func (m *fadeModel) step(now time.Time) (float64, bool) {
	for {
		select {
		case c := <-m.cmds:
			m.from, _ = fadeAlpha(m.from, m.to, m.start, m.dur, now)
			m.to = 0
			if c.black {
				m.to = 1
			}
			m.start, m.dur, m.started = now, c.dur, true
			// Any wake at or after the end marks every surface dirty until it
			// draws, so that draw shows the final opacity. The extra ticks are
			// margin for ticker jitter.
			m.tickUntil.Store(int64(now.Add(c.dur + 2*fadeTick).Sub(m.base)))
			continue
		default:
		}
		break
	}
	return fadeAlpha(m.from, m.to, m.start, m.dur, now)
}

// fadeRunner runs the overlay until ctx ends or a reveal completes.
type fadeRunner interface {
	RunFade(ctx context.Context, m *fadeModel) error
}

// guiFader renders the overlay as one click-through layer-shell surface per
// output, on its own Wayland connection.
type guiFader struct{}

func (guiFader) RunFade(ctx context.Context, m *fadeModel) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.finish = cancel
	conn, err := neferclient.Connect(ctx, "")
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("connect: %w", err)
	}
	f := &fader{m: m}
	f.screens = newScreens(conn)
	defer func() { err = errors.Join(err, conn.Close(), f.closeAll()) }()
	for _, o := range conn.Outputs() {
		if err = f.cover(o); err != nil {
			return err
		}
	}
	go fadeTicker(ctx, m)
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil // cancelled, or the reveal finished
		case <-conn.Wake():
			if err = conn.Dispatch(f); err != nil {
				return fmt.Errorf("dispatch: %w", err)
			}
		case <-m.wake:
			f.invalidateAll()
		case <-retry:
		}
		if f.err != nil {
			return f.err
		}
		pending, err := f.drawAll()
		if err != nil {
			return err
		}
		retry = nil
		if pending {
			retry = time.After(gpuRetry)
		}
	}
}

type fader struct {
	*screens
	m *fadeModel
}

// cover adds the overlay surface of an output.
func (f *fader) cover(o neferclient.Output) error {
	surf, err := f.conn.NewLayerSurface(fadeLayer(o.Name))
	if err != nil {
		return fmt.Errorf("fade surface for output %s: %w", o.Name, err)
	}
	s := newScreen(f.conn, surf, o, true)
	s.view = func(fr *nefergui.Frame) { fadeView(fr, f.m) }
	f.add(s)
	return nil
}

// OutputAdded covers an output that appeared during the fade.
func (f *fader) OutputAdded(o *neferclient.Output) {
	if !f.covers(o.Global) {
		if err := f.cover(*o); err != nil {
			f.fail(err)
		}
	}
}

// Closed: the compositor closed an overlay surface.
func (f *fader) Closed(id neferclient.SurfaceID) {
	if s := f.byID[id]; s != nil {
		if err := f.forget(s); err != nil {
			f.fail(err)
		}
	}
}

// fadeTicker requests redraws while the fade animates.
func fadeTicker(ctx context.Context, m *fadeModel) {
	t := time.NewTicker(fadeTick)
	defer t.Stop()
	was := false
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			// One more poke when animation stops: even a starved ticker then
			// wakes every surface at least once past the end.
			animating := m.animatingAt(now)
			if animating || was {
				m.poke()
			}
			was = animating
		}
	}
}

// fadeLayer: overlay on one output, all anchors, no keyboard, fully
// click-through. An empty output name lets the compositor choose.
func fadeLayer(output string) neferclient.LayerConfig {
	return neferclient.LayerConfig{
		Output:        output,
		Namespace:     "neferafk-fade",
		Level:         neferclient.LayerOverlay,
		Anchors:       neferclient.AnchorTop | neferclient.AnchorBottom | neferclient.AnchorLeft | neferclient.AnchorRight,
		Keyboard:      neferclient.KeyboardNone,
		ExclusiveZone: -1,
		InputRects:    []neferclient.Rect{},
	}
}

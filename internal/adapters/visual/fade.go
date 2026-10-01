package visual

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

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
	// tickUntil (UnixNano) keeps redraws coming until every surface has drawn
	// a frame after the fade ended. It must not depend on which surface drew
	// last: a wake redraws all surfaces, but each draws at its own instant.
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
	return &fadeModel{cmds: make(chan fadeCmd, 4), wake: make(chan struct{}, 1)}
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
	return now.UnixNano() < m.tickUntil.Load()
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
			// Two ticks past the end: every surface draws the final opacity.
			m.tickUntil.Store(now.Add(c.dur + 2*fadeTick).UnixNano())
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

// guiFader renders the overlay as a click-through layer-shell surface.
type guiFader struct{}

func (guiFader) RunFade(ctx context.Context, m *fadeModel) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.finish = cancel
	go func() {
		t := time.NewTicker(fadeTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				if m.animatingAt(now) {
					m.poke()
				}
			}
		}
	}()
	err := nefergui.Run(ctx, m, fadeView,
		nefergui.Title("neferafk-fade"),
		nefergui.Size(64, 64),
		nefergui.Transparent(),
		nefergui.Wake(m.wake),
		nefergui.Layer(fadeLayer()),
	)
	if ctx.Err() != nil && err == context.Canceled {
		return nil
	}
	return err
}

// fadeLayer: overlay on every output, all anchors, no keyboard, fully
// click-through.
func fadeLayer() nefergui.LayerConfig {
	return nefergui.LayerConfig{
		AllOutputs:    true,
		Namespace:     "neferafk-fade",
		Level:         nefergui.LayerOverlay,
		Anchors:       nefergui.AnchorTop | nefergui.AnchorBottom | nefergui.AnchorLeft | nefergui.AnchorRight,
		Keyboard:      nefergui.KeyboardNone,
		ExclusiveZone: -1,
		InputRects:    []nefergui.Rect{},
	}
}

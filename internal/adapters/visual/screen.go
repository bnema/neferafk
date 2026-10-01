package visual

import (
	"errors"
	"fmt"
	"time"

	"github.com/bnema/neferclient"
	"github.com/bnema/nefergui"
)

// screen is one Wayland surface and the NeferGUI renderer that draws it. It
// copies plain fields between the two libraries, which never import each
// other. Everything runs on the owner goroutine that calls Conn.Dispatch.
type screen struct {
	conn        *neferclient.Conn
	surf        *neferclient.Surface
	output      uint32 // wl_output registry name
	name        string // connector name, for error reports
	transparent bool   // ARGB buffers, presented as non-opaque
	view        func(*nefergui.Frame)

	r      *nefergui.Renderer
	fb     *neferclient.Feedback // feedback r was built from
	out    nefergui.Output
	damage []neferclient.Rect
	live   map[uint64]int // buffer → release eventfd still watched

	configured, canPresent, haveAcquire bool
}

func newScreen(conn *neferclient.Conn, surf *neferclient.Surface, o neferclient.Output, transparent bool) *screen {
	return &screen{conn: conn, surf: surf, output: o.Global, name: o.Name, transparent: transparent, live: map[uint64]int{}}
}

// renderView adapts the per-screen view to Renderer.Render without a
// per-frame closure.
func renderView(f *nefergui.Frame, s *screen) { s.view(f) }

// configure applies Handler.Configure. Only the first one lifts the
// frame-callback gate; later ones must wait for Handler.Frame.
func (s *screen) configure() error {
	if !s.configured {
		s.canPresent = true
	}
	s.configured = true
	s.resize()
	return s.setup()
}

// setup builds the renderer once the surface is configured and its dmabuf
// feedback is complete, and rebuilds it when the compositor changes its
// preferred device or formats (a repeated round is not a change).
func (s *screen) setup() error {
	fb := s.surf.Feedback()
	if !s.configured || fb == nil || s.r != nil && fb.Equal(s.fb) {
		return nil
	}
	if err := s.dropRenderer(); err != nil {
		return err
	}
	cfg := nefergui.RendererConfig{MainDevice: fb.MainDevice, Formats: make([]nefergui.Format, len(fb.Formats)), Transparent: s.transparent}
	for i, f := range fb.Formats {
		cfg.Formats[i] = nefergui.Format{FourCC: f.FourCC, Modifier: f.Modifier}
	}
	r, err := nefergui.NewRenderer(cfg)
	if err != nil {
		return fmt.Errorf("renderer: %w", err)
	}
	s.r, s.fb = r, fb.Clone() // Feedback storage is reused by later rounds
	s.resize()
	return nil
}

func (s *screen) resize() {
	if s.r != nil {
		w, h, scale := s.surf.Size()
		s.r.Resize(int(w), int(h), scale)
	}
}

func (s *screen) invalidate() {
	if s.r != nil {
		s.r.Invalidate()
	}
}

// draw renders when something changed and presents the frame. It reports
// whether a built frame still waits for the GPU.
func (s *screen) draw() (pending bool, err error) {
	if s.r == nil {
		return false, nil
	}
	if s.canPresent {
		ok, err := s.r.Render(&s.out, s, renderView)
		if err != nil {
			return false, fmt.Errorf("render: %w", err)
		}
		if ok {
			if err := s.present(); err != nil {
				return false, err
			}
		}
	}
	return s.r.Pending(), nil
}

// watchID packs the surface and a buffer into the Conn.WatchFD id.
func (s *screen) watchID(buffer uint64) uint64 { return uint64(s.surf.ID())<<32 | buffer }

// present: Renderer output → ImportBuffer, ImportTimeline, Present. The input
// region is not forwarded: it is fixed when the surface is created.
func (s *screen) present() error {
	out := &s.out
	for _, rt := range out.Retired { // buffers of an older size
		if err := s.conn.UnwatchFD(rt.ReleaseFD); err != nil {
			return fmt.Errorf("unwatch retired buffer %d: %w", rt.Buffer, err)
		}
		delete(s.live, rt.Buffer)
		if err := s.surf.DestroyBuffer(rt.Buffer); err != nil {
			return fmt.Errorf("destroy retired buffer %d: %w", rt.Buffer, err)
		}
		// The release timeline of a buffer is imported under the buffer's id.
		if err := s.surf.DestroyTimeline(rt.Buffer); err != nil {
			return fmt.Errorf("destroy retired timeline %d: %w", rt.Buffer, err)
		}
	}
	if out.NewBuffer {
		buf := neferclient.Buffer{Width: out.Width, Height: out.Height, FourCC: out.FourCC, Modifier: out.Modifier, PlaneCount: out.PlaneCount}
		for i, p := range out.Planes {
			buf.Planes[i] = neferclient.Plane{FD: p.FD, Offset: p.Offset, Stride: p.Stride}
		}
		if err := s.surf.ImportBuffer(out.Buffer, &buf); err != nil {
			return fmt.Errorf("import buffer: %w", err)
		}
		// The release eventfd is stable per buffer: watched once, FDReady
		// hands the buffer back with Renderer.Released.
		if err := s.conn.WatchFD(out.ReleaseFD, s.watchID(out.Buffer)); err != nil {
			return fmt.Errorf("watch release fd: %w", err)
		}
		s.live[out.Buffer] = out.ReleaseFD
	}
	if out.NewTimelines {
		if !s.haveAcquire { // one acquire timeline shared by every buffer
			if err := s.surf.ImportTimeline(out.Acquire.ID, out.Acquire.FD); err != nil {
				return fmt.Errorf("import acquire timeline: %w", err)
			}
			s.haveAcquire = true
		}
		if err := s.surf.ImportTimeline(out.Release.ID, out.Release.FD); err != nil {
			return fmt.Errorf("import release timeline: %w", err)
		}
	}
	s.damage = s.damage[:0]
	for _, d := range out.Damage {
		s.damage = append(s.damage, neferclient.Rect{X: d.X, Y: d.Y, Width: d.Width, Height: d.Height})
	}
	if err := s.surf.Present(&neferclient.Present{
		Buffer: out.Buffer, AcquireTimeline: out.Acquire.ID, ReleaseTimeline: out.Release.ID,
		AcquirePoint: out.AcquirePoint, ReleasePoint: out.ReleasePoint, Damage: s.damage,
		Opaque: !s.transparent,
	}); err != nil {
		return fmt.Errorf("present: %w", err)
	}
	s.canPresent = false // until Handler.Frame
	return nil
}

// released hands a buffer back after its release eventfd became readable.
func (s *screen) released(buffer uint64) error {
	if s.r == nil {
		return nil
	}
	if err := s.r.Released(buffer); err != nil {
		return fmt.Errorf("release buffer: %w", err)
	}
	return nil
}

// dropRenderer stops watching the release eventfds (they belong to the
// renderer), destroys the imported buffers and timelines (allowed while a
// frame is pending), then closes the renderer.
func (s *screen) dropRenderer() error {
	if s.r == nil {
		return nil
	}
	var errs []error
	for buffer, fd := range s.live {
		if err := s.conn.UnwatchFD(fd); err != nil {
			errs = append(errs, fmt.Errorf("unwatch buffer %d: %w", buffer, err))
		}
	}
	clear(s.live)
	errs = append(errs, s.surf.DestroyImports(), s.r.Close())
	s.r, s.fb, s.haveAcquire = nil, nil, false
	return errors.Join(errs...)
}

// close frees the renderer and the surface.
func (s *screen) close() error {
	return errors.Join(s.dropRenderer(), s.surf.Close())
}

// closeAfterConn frees what is left once the connection is closed: the
// connection dropped the descriptor watches and every Wayland object, so only
// the renderer's GPU resources remain.
func (s *screen) closeAfterConn() error {
	if s.r == nil {
		return nil
	}
	err := s.r.Close()
	s.r = nil
	return err
}

// screens holds the surfaces of one connection and implements the Handler
// methods they share. Failures from handler methods are kept in err; the
// owner loop returns it after Dispatch.
type screens struct {
	neferclient.NopHandler

	conn *neferclient.Conn
	byID map[neferclient.SurfaceID]*screen
	err  error
	// onSetup reports a screen whose renderer could not be set up; it returns
	// the error that ends the run, or nil to drop only that screen.
	onSetup func(s *screen, err error) error
}

func newScreens(conn *neferclient.Conn) *screens {
	ss := &screens{conn: conn, byID: map[neferclient.SurfaceID]*screen{}}
	ss.onSetup = func(_ *screen, err error) error { return err }
	return ss
}

func (ss *screens) fail(err error) {
	if ss.err == nil {
		ss.err = err
	}
}

func (ss *screens) add(s *screen) { ss.byID[s.surf.ID()] = s }

// forget drops a screen and frees it.
func (ss *screens) forget(s *screen) error {
	delete(ss.byID, s.surf.ID())
	return s.close()
}

func (ss *screens) setupFailed(s *screen, err error) {
	if err = ss.onSetup(s, err); err != nil {
		ss.fail(err)
		return
	}
	if err := ss.forget(s); err != nil {
		ss.fail(err)
	}
}

func (ss *screens) Configure(id neferclient.SurfaceID, _, _ int32) {
	if s := ss.byID[id]; s != nil {
		if err := s.configure(); err != nil {
			ss.setupFailed(s, err)
		}
	}
}

func (ss *screens) FeedbackDone(id neferclient.SurfaceID) {
	if s := ss.byID[id]; s != nil {
		if err := s.setup(); err != nil {
			ss.setupFailed(s, err)
		}
	}
}

func (ss *screens) Scale(id neferclient.SurfaceID, _ float64) {
	if s := ss.byID[id]; s != nil {
		s.resize()
	}
}

func (ss *screens) Frame(id neferclient.SurfaceID) {
	if s := ss.byID[id]; s != nil {
		s.canPresent = true
	}
}

func (ss *screens) FDReady(id uint64) {
	if s := ss.byID[neferclient.SurfaceID(id>>32)]; s != nil {
		if err := s.released(id & 0xffffffff); err != nil {
			ss.fail(err)
		}
	}
}

// OutputRemoved frees the screens of an output that went away.
func (ss *screens) OutputRemoved(global uint32) {
	for _, s := range ss.byID {
		if s.output == global {
			if err := ss.forget(s); err != nil {
				ss.fail(fmt.Errorf("output %s removed: %w", s.name, err))
			}
		}
	}
}

// covers reports whether an output already has a screen.
func (ss *screens) covers(global uint32) bool {
	for _, s := range ss.byID {
		if s.output == global {
			return true
		}
	}
	return false
}

func (ss *screens) invalidateAll() {
	for _, s := range ss.byID {
		s.invalidate()
	}
}

// drawAll draws every screen and reports whether one waits for the GPU.
func (ss *screens) drawAll() (pending bool, err error) {
	for _, s := range ss.byID {
		p, err := s.draw()
		if err != nil {
			return false, fmt.Errorf("output %s: %w", s.name, err)
		}
		pending = pending || p
	}
	return pending, nil
}

// closeAll frees every screen after the connection was closed.
func (ss *screens) closeAll() error {
	var errs []error
	for id, s := range ss.byID {
		delete(ss.byID, id)
		errs = append(errs, s.closeAfterConn())
	}
	return errors.Join(errs...)
}

// gpuRetry is how soon to render again while a frame waits for the GPU; no
// event announces that the GPU is done.
const gpuRetry = 2 * time.Millisecond

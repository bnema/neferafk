package wayland

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/zerowrap"
	"golang.org/x/sys/unix"
)

func sockets(t *testing.T) (net.Conn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	files := []*os.File{os.NewFile(uintptr(fds[0]), "client"), os.NewFile(uintptr(fds[1]), "peer")}
	a, err := net.FileConn(files[0])
	files[0].Close()
	if err != nil {
		t.Fatal(err)
	}
	b, err := net.FileConn(files[1])
	files[1].Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b.(*net.UnixConn)
}
func packet(id uint32, op uint16, body []byte) []byte {
	b := make([]byte, 8+len(body))
	binary.NativeEndian.PutUint32(b, id)
	binary.NativeEndian.PutUint32(b[4:], uint32(len(b))<<16|uint32(op))
	copy(b[8:], body)
	return b
}
func words(v ...uint32) []byte {
	b := make([]byte, 4*len(v))
	for i, n := range v {
		binary.NativeEndian.PutUint32(b[i*4:], n)
	}
	return b
}
func wireString(s string) []byte {
	b := make([]byte, 4+(len(s)+4)&^3)
	binary.NativeEndian.PutUint32(b, uint32(len(s)+1))
	copy(b[4:], s)
	return b
}
func readWire(t *testing.T, p *net.UnixConn) (uint32, uint16, []byte) {
	t.Helper()
	p.SetReadDeadline(time.Now().Add(2 * time.Second))
	h := make([]byte, 8)
	if _, err := io.ReadFull(p, h); err != nil {
		t.Fatal(err)
	}
	size := binary.NativeEndian.Uint32(h[4:]) >> 16
	b := make([]byte, size-8)
	if _, err := io.ReadFull(p, b); err != nil {
		t.Fatal(err)
	}
	return binary.NativeEndian.Uint32(h), uint16(binary.NativeEndian.Uint32(h[4:])), b
}
func sendWire(t *testing.T, p *net.UnixConn, id uint32, op uint16, b []byte) {
	t.Helper()
	if _, err := p.Write(packet(id, op, b)); err != nil {
		t.Fatal(err)
	}
}
func global(t *testing.T, p *net.UnixConn, registry, name uint32, iface string, v uint32) {
	b := append(words(name), wireString(iface)...)
	b = append(b, words(v)...)
	sendWire(t, p, registry, 0, b)
}
func synchronize(t *testing.T, p *net.UnixConn, body []byte) {
	id := binary.NativeEndian.Uint32(body)
	sendWire(t, p, id, 0, words(1))
	sendWire(t, p, 1, 1, words(id))
}
func capability(t *testing.T, out <-chan ports.WaylandEvent) ports.Capabilities {
	t.Helper()
	for {
		select {
		case e := <-out:
			if c, ok := e.(ports.WaylandCapabilities); ok {
				return c.Capabilities
			}
		case <-time.After(2 * time.Second):
			t.Fatal("capability timeout")
		}
	}
}
func nextEvent(t *testing.T, out <-chan ports.WaylandEvent) ports.WaylandEvent {
	t.Helper()
	select {
	case e := <-out:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("event timeout")
		return nil
	}
}

// started is a client running against a fake compositor that announced
// its globals and answered the initial bindings (one output, HEADLESS-1).
type started struct {
	peer                  *net.UnixConn
	registry, wake, power uint32
	ids                   map[string]uint32 // bound ID by interface
	notifications         map[uint32]uint32 // idle notification by timeout (ms)
	commands              chan ports.WaylandCommand
	out                   chan ports.WaylandEvent
	done                  chan error
}

// extraGlobal is one more global announced at start.
type extraGlobal struct {
	name    uint32
	iface   string
	version uint32
}

// start runs a client and the initial discovery; extra globals follow
// wl_seat, wl_output, ext_idle_notifier_v1 and zwlr_output_power_manager_v1.
func start(t *testing.T, ctx context.Context, options Options, extra ...extraGlobal) *started {
	t.Helper()
	conn, peer := sockets(t)
	c, err := New(conn, options, zerowrap.New(zerowrap.Config{Output: io.Discard}))
	if err != nil {
		t.Fatal(err)
	}
	_, _, body := readWire(t, peer)
	s := &started{peer: peer, registry: binary.NativeEndian.Uint32(body), ids: map[string]uint32{}, notifications: map[uint32]uint32{}, commands: make(chan ports.WaylandCommand, 8), out: make(chan ports.WaylandEvent, 64), done: make(chan error, 1)}
	go func() { s.done <- c.Run(ctx, s.commands, s.out) }()
	_, _, body = readWire(t, peer)
	global(t, peer, s.registry, 10, "wl_seat", 5)
	global(t, peer, s.registry, 11, "wl_output", 4)
	global(t, peer, s.registry, 12, "ext_idle_notifier_v1", 2)
	global(t, peer, s.registry, 13, "zwlr_output_power_manager_v1", 1)
	for _, g := range extra {
		global(t, peer, s.registry, g.name, g.iface, g.version)
	}
	synchronize(t, peer, body)
	for {
		id, op, b := readWire(t, peer)
		if id == 1 {
			synchronize(t, peer, b)
			break
		}
		if id == s.registry {
			n := int(binary.NativeEndian.Uint32(b[4:]))
			iface := string(b[8 : 8+n-1])
			s.ids[iface] = binary.NativeEndian.Uint32(b[len(b)-4:])
			if iface == "wl_output" {
				sendWire(t, peer, s.ids[iface], 4, wireString("HEADLESS-1"))
			}
			continue
		}
		if id == s.ids["ext_idle_notifier_v1"] {
			child := binary.NativeEndian.Uint32(b)
			if op == 2 {
				s.wake = child
			} else {
				s.notifications[binary.NativeEndian.Uint32(b[4:])] = child
			}
		}
		if id == s.ids["zwlr_output_power_manager_v1"] {
			s.power = binary.NativeEndian.Uint32(b)
			sendWire(t, peer, s.power, 0, words(1))
		}
	}
	return s
}

func TestDiscoveryIdleReloadPowerAndRemoval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := start(t, ctx, Options{Requirements: ports.WaylandRequirements{Idle: true, InputWake: true, OutputPower: true, Lock: true, Visual: true}, Generation: 1, Deadlines: []time.Duration{time.Second, time.Second, 2 * time.Second}}, extraGlobal{14, "ext_session_lock_manager_v1", 1})
	peer, registry, ids, notifications, wake, power := s.peer, s.registry, s.ids, s.notifications, s.wake, s.power
	commands, out, done := s.commands, s.out, s.done
	caps := capability(t, out)
	if len(notifications) != 2 || wake == 0 || power == 0 || len(caps.Outputs) != 1 || caps.Outputs[0].Name != "HEADLESS-1" || !caps.Outputs[0].PowerSupported {
		t.Fatalf("initial: notifications=%v wake=%d power=%d caps=%+v", notifications, wake, power, caps)
	}
	if !slices.Contains(caps.Missing, "zwp_linux_dmabuf_v1 >= 4") || slices.Contains(caps.Missing, "ext_session_lock_manager_v1 >= 1") {
		t.Fatal(caps.Missing)
	}
	lifetime := caps.Outputs[0].Lifetime
	sendWire(t, peer, notifications[1000], 0, nil)
	idle, ok := nextEvent(t, out).(ports.WaylandIdle)
	if !ok || idle.Generation != 1 || idle.After != time.Second || idle.Cycle != 1 {
		t.Fatal(idle, ok)
	}
	// Reload retains the one-second proxy and removes only the two-second one.
	commands <- ports.SetDeadlines{Generation: 2, Deadlines: []time.Duration{time.Second, 3 * time.Second}}
	var added uint32
	for i := 0; i < 2; i++ {
		id, op, b := readWire(t, peer)
		if id == notifications[2000] && op == 0 {
			continue
		}
		if id == ids["ext_idle_notifier_v1"] && op == 1 && binary.NativeEndian.Uint32(b[4:]) == 3000 {
			added = binary.NativeEndian.Uint32(b)
			continue
		}
		t.Fatalf("reload request %d/%d %x", id, op, b)
	}
	if added == 0 {
		t.Fatal("new deadline missing")
	}
	capability(t, out)
	// An in-flight event for the deleted notification must never become policy.
	sendWire(t, peer, notifications[2000], 0, nil)
	sendWire(t, peer, notifications[1000], 1, nil)
	sendWire(t, peer, notifications[1000], 0, nil)
	idle, ok = nextEvent(t, out).(ports.WaylandIdle)
	if !ok || idle.Generation != 2 || idle.Proxy == 0 || idle.Cycle != 2 {
		t.Fatal(idle, ok)
	}
	commands <- ports.OutputPower{On: false}
	id, op, b := readWire(t, peer)
	if id != power || op != 0 || binary.NativeEndian.Uint32(b) != 0 {
		t.Fatalf("off %d/%d %x", id, op, b)
	}
	sendWire(t, peer, wake, 0, nil)
	sendWire(t, peer, wake, 1, nil)
	id, op, b = readWire(t, peer)
	if id != power || op != 0 || binary.NativeEndian.Uint32(b) != 1 {
		t.Fatalf("wake %d/%d %x", id, op, b)
	}
	if _, ok := nextEvent(t, out).(ports.WaylandActivity); !ok {
		t.Fatal("missing activity")
	}
	// Removing/re-adding the same global number creates a new output lifetime.
	sendWire(t, peer, registry, 1, words(11))
	for i := 0; i < 2; i++ {
		readWire(t, peer)
	}
	caps = capability(t, out)
	if len(caps.Outputs) != 0 {
		t.Fatal(caps)
	}
	global(t, peer, registry, 11, "wl_output", 4)
	var newPower uint32
	for i := 0; i < 2; i++ {
		id, _, b = readWire(t, peer)
		if id == registry {
			ids["new-output"] = binary.NativeEndian.Uint32(b[len(b)-4:])
		} else {
			newPower = binary.NativeEndian.Uint32(b)
		}
	}
	caps = capability(t, out)
	if len(caps.Outputs) != 1 || caps.Outputs[0].Lifetime == lifetime {
		t.Fatal(caps)
	}
	// A late failed event from the previous output cannot disable its successor.
	sendWire(t, peer, power, 1, nil)
	sendWire(t, peer, newPower, 1, nil)
	id, op, _ = readWire(t, peer)
	if id != newPower || op != 1 {
		t.Fatal("failed control not destroyed", id, op)
	}
	if failure, ok := nextEvent(t, out).(ports.WaylandPowerFailed); !ok || failure.Output != caps.Outputs[0].Lifetime {
		t.Fatal(failure, ok)
	}
	caps = capability(t, out)
	if caps.Outputs[0].PowerSupported || len(caps.Missing) != 4 {
		t.Fatal(caps)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run failed to join reader")
	}
}

func TestOutputAddedWhileOffIsTurnedOffAndWakesWithTheOthers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := start(t, ctx, Options{Requirements: ports.WaylandRequirements{Idle: true, InputWake: true, OutputPower: true}, Generation: 1, Deadlines: []time.Duration{time.Second}})
	peer, registry, wake, power := s.peer, s.registry, s.wake, s.power
	commands, out, done := s.commands, s.out, s.done
	manager := s.ids["zwlr_output_power_manager_v1"]
	capability(t, out)
	if manager == 0 || wake == 0 || power == 0 {
		t.Fatal(manager, wake, power)
	}
	commands <- ports.OutputPower{On: false}
	if id, op, b := readWire(t, peer); id != power || op != 0 || binary.NativeEndian.Uint32(b) != 0 {
		t.Fatalf("off %d/%d %x", id, op, b)
	}
	// addOutput announces an output and answers its power control's initial
	// mode; it returns that control.
	addOutput := func(name, mode uint32) uint32 {
		global(t, peer, registry, name, "wl_output", 4)
		var p uint32
		for i := 0; i < 2; i++ {
			if id, _, b := readWire(t, peer); id == manager {
				p = binary.NativeEndian.Uint32(b)
			}
		}
		sendWire(t, peer, p, 0, words(mode))
		return p
	}
	// A display reconnecting from deep sleep comes back on: turned off.
	on := addOutput(14, 1)
	if id, op, b := readWire(t, peer); id != on || op != 0 || binary.NativeEndian.Uint32(b) != 0 {
		t.Fatalf("new output off %d/%d %x", id, op, b)
	}
	// One that comes back off needs no request, but is woken with the rest.
	off := addOutput(15, 0)
	sendWire(t, peer, wake, 0, nil)
	sendWire(t, peer, wake, 1, nil)
	woken := map[uint32]bool{}
	for range 3 {
		id, op, b := readWire(t, peer)
		if op != 0 || binary.NativeEndian.Uint32(b) != 1 {
			t.Fatalf("wake %d/%d %x", id, op, b)
		}
		woken[id] = true
	}
	if !woken[power] || !woken[on] || !woken[off] {
		t.Fatal(woken)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMissingAndDisabledCapabilities(t *testing.T) {
	conn, peer := sockets(t)
	c, err := New(conn, Options{Requirements: ports.WaylandRequirements{Lock: true, Visual: true}}, zerowrap.New(zerowrap.Config{Output: io.Discard}))
	if err != nil {
		t.Fatal(err)
	}
	_, _, b := readWire(t, peer)
	registry := binary.NativeEndian.Uint32(b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan ports.WaylandEvent, 8)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, nil, out) }()
	_, _, b = readWire(t, peer)
	global(t, peer, registry, 10, "ext_idle_notifier_v1", 1)
	global(t, peer, registry, 11, "zwlr_output_power_manager_v1", 1)
	synchronize(t, peer, b)
	id, _, b := readWire(t, peer)
	if id != 1 {
		t.Fatalf("disabled optional global bound: %d", id)
	}
	synchronize(t, peer, b)
	caps := capability(t, out)
	if len(caps.Missing) != 5 || slices.Contains(caps.Missing, "ext_idle_notifier_v1 >= 2") {
		t.Fatal(caps)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDeadlineValidation(t *testing.T) {
	for _, d := range []time.Duration{-time.Second, time.Nanosecond, time.Duration(1<<32) * time.Millisecond} {
		if _, err := normalize([]time.Duration{d}); err == nil {
			t.Fatal(d)
		}
	}
	ds, err := normalize([]time.Duration{0, time.Second, time.Second})
	if err != nil || len(ds) != 1 {
		t.Fatal(ds, err)
	}
}

func TestCancelBlockedStartup(t *testing.T) {
	conn, peer := sockets(t)
	c, err := New(conn, Options{}, zerowrap.New(zerowrap.Config{Output: io.Discard}))
	if err != nil {
		t.Fatal(err)
	}
	readWire(t, peer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, nil, nil) }()
	readWire(t, peer) // sync request deliberately unanswered
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not close socket")
	}
}

func TestBoundedCallbackQueueFailsClosed(t *testing.T) {
	conn, peer := sockets(t)
	c, err := New(conn, Options{}, zerowrap.New(zerowrap.Config{Output: io.Discard}))
	if err != nil {
		t.Fatal(err)
	}
	readWire(t, peer)
	for i := 0; i <= queueSize; i++ {
		c.enqueue(notice{kind: 1})
	}
	select {
	case <-c.overflow:
	default:
		t.Fatal("overflow not reported")
	}
}

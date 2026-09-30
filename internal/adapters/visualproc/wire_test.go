package visualproc

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	"github.com/bnema/neferafk/internal/ports"
)

func TestCommandRoundTrip(t *testing.T) {
	var b bytes.Buffer
	lock := ports.VisualLock{Generation: 4, Auth: ports.AuthBootstrap{Generation: 4, PINSource: ports.PINSourceEnv, PINReference: "r", EnvPIN: []byte("123456")}}
	for _, c := range []ports.VisualCommand{ports.VisualFade{Black: true, Duration: 2 * time.Second}, ports.VisualFade{}, lock} {
		if err := WriteCommand(&b, c); err != nil {
			t.Fatal(err)
		}
	}
	if c, err := ReadCommand(&b); err != nil || !reflect.DeepEqual(c, ports.VisualFade{Black: true, Duration: 2 * time.Second}) {
		t.Fatalf("%#v %v", c, err)
	}
	if c, err := ReadCommand(&b); err != nil || !reflect.DeepEqual(c, ports.VisualFade{}) {
		t.Fatalf("%#v %v", c, err)
	}
	c, err := ReadCommand(&b)
	got, ok := c.(ports.VisualLock)
	if err != nil || !ok || got.Generation != 4 || got.Auth.Generation != 4 || got.Auth.PINSource != ports.PINSourceEnv || got.Auth.PINReference != "r" || string(got.Auth.EnvPIN) != "123456" {
		t.Fatalf("%#v %v", c, err)
	}
}

func TestCommandRejectsMismatchedGenerationAndGarbage(t *testing.T) {
	var b bytes.Buffer
	if err := WriteCommand(&b, ports.VisualLock{Generation: 1, Auth: ports.AuthBootstrap{Generation: 2}}); err == nil {
		t.Fatal("mismatched generations encoded")
	}
	if err := WriteCommand(&b, ports.VisualLock{}); err == nil {
		t.Fatal("zero generation encoded")
	}
	for _, in := range []string{"", "NAV1", "XXXX\x01\x00\x00\x00\x00", "NAV1\x09\x00\x00\x00\x00", "NAV1\x01\xff\xff\xff\xff", "NAV1\x01\x00\x00\x00\x02ab"} {
		if _, err := ReadCommand(bytes.NewReader([]byte(in))); err == nil {
			t.Fatalf("%q accepted", in)
		}
		if _, err := ReadEvent(bytes.NewReader([]byte(in))); err == nil {
			t.Fatalf("%q accepted as event", in)
		}
	}
}

func TestEventRoundTripAndNoUnlockRequestExists(t *testing.T) {
	var b bytes.Buffer
	for _, e := range []ports.VisualEvent{ports.LockConfirmed{Generation: 2}, ports.LockReleased{Generation: 2}} {
		if err := WriteEvent(&b, e); err != nil {
			t.Fatal(err)
		}
	}
	if e, err := ReadEvent(&b); err != nil || e != (ports.LockConfirmed{Generation: 2}) {
		t.Fatalf("%#v %v", e, err)
	}
	if e, err := ReadEvent(&b); err != nil || e != (ports.LockReleased{Generation: 2}) {
		t.Fatalf("%#v %v", e, err)
	}
	if err := WriteEvent(&b, ports.VisualExited{}); err == nil {
		t.Fatal("process exit must not be a wire event")
	}
	// A daemon->visual frame kind is not accepted in the event direction, and
	// there is no command kind other than fade and lock.
	var c bytes.Buffer
	WriteCommand(&c, ports.VisualFade{})
	if _, err := ReadEvent(&c); err == nil {
		t.Fatal("command parsed as event")
	}
	if _, err := ReadCommand(bytes.NewReader([]byte("NAV1\x04\x00\x00\x00\x08\x00\x00\x00\x00\x00\x00\x00\x01"))); err == nil {
		t.Fatal("event parsed as command")
	}
}

func TestReadCommandBoundsPINSource(t *testing.T) {
	// Hand-build a lock frame whose PINSource is 17 bytes: the writer refuses
	// it, so craft the payload directly.
	p := binary.BigEndian.AppendUint64(nil, 1)
	for _, f := range [][]byte{bytes.Repeat([]byte("x"), 17), nil, nil} {
		p = binary.BigEndian.AppendUint32(p, uint32(len(f)))
		p = append(p, f...)
	}
	var b bytes.Buffer
	if err := writeFrame(&b, kindLock, p); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCommand(&b); err == nil {
		t.Fatal("oversize PINSource accepted")
	}
}

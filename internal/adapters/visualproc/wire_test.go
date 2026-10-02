package visualproc

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bnema/neferafk/internal/ports"
)

func TestCommandRoundTrip(t *testing.T) {
	var b bytes.Buffer
	lock := ports.VisualLock{Generation: 4, Auth: ports.AuthBootstrap{Generation: 4, PINSource: ports.PINSourceEnv, PINReference: "r", EnvPIN: []byte("123456")}, Output: "DP-2"}
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
	if err != nil || !ok || got.Generation != 4 || got.Auth.Generation != 4 || got.Auth.PINSource != ports.PINSourceEnv || got.Auth.PINReference != "r" || string(got.Auth.EnvPIN) != "123456" || got.Output != "DP-2" {
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
	if err := WriteCommand(&b, ports.VisualLock{Generation: 1, Auth: ports.AuthBootstrap{Generation: 1}, Output: strings.Repeat("x", ports.MaxOutputName+1)}); err == nil {
		t.Fatal("oversized output name encoded")
	}
	for _, in := range []string{"", "NAV2", "XXXX\x01\x00\x00\x00\x00", "NAV1\x01\x00\x00\x00\x09\x00\x00\x00\x00\x00\x00\x00\x00\x00", "NAV2\x09\x00\x00\x00\x00", "NAV2\x01\xff\xff\xff\xff", "NAV2\x01\x00\x00\x00\x02ab"} {
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
	if _, err := ReadCommand(bytes.NewReader([]byte("NAV2\x04\x00\x00\x00\x08\x00\x00\x00\x00\x00\x00\x00\x01"))); err == nil {
		t.Fatal("event parsed as command")
	}
}

func TestReadCommandBoundsLockFields(t *testing.T) {
	// Hand-build lock frames the writer refuses: an oversize PINSource or
	// Output, and the older three-field layout.
	long := func(n int) []byte { return bytes.Repeat([]byte("x"), n) }
	for name, fields := range map[string][][]byte{
		"oversize PINSource": {long(17), nil, nil, nil},
		"oversize Output":    {nil, nil, nil, long(ports.MaxOutputName + 1)},
		"three fields":       {nil, nil, nil},
	} {
		p := binary.BigEndian.AppendUint64(nil, 1)
		for _, f := range fields {
			p = binary.BigEndian.AppendUint32(p, uint32(len(f)))
			p = append(p, f...)
		}
		var b bytes.Buffer
		if err := writeFrame(&b, kindLock, p); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadCommand(&b); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

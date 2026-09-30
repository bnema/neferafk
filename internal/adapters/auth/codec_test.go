package auth

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/bnema/neferafk/internal/ports"
)

func TestCodecBoundsAndTruncation(t *testing.T) {
	f := ports.AuthFrame{Kind: ports.AuthSecret, Generation: 7, Attempt: ^uint64(0), Payload: bytes.Repeat([]byte{'x'}, 512)}
	var wire bytes.Buffer
	if err := WriteFrame(&wire, f); err != nil {
		t.Fatal(err)
	}
	encoded := bytes.Clone(wire.Bytes())
	got, err := ReadFrame(&wire)
	if err != nil || got.Generation != 7 || got.Attempt != f.Attempt || !bytes.Equal(got.Payload, f.Payload) {
		t.Fatalf("roundtrip %v", err)
	}
	clear(got.Payload)
	for _, n := range []int{1, 4, 10, 26, len(encoded) - 1} {
		if _, err := ReadFrame(bytes.NewReader(encoded[:n])); err == nil {
			t.Fatalf("truncation %d accepted", n)
		}
	}
	for _, corrupt := range []string{"magic", "kind", "generation", "secret-length", "huge"} {
		b := bytes.Clone(encoded)
		switch corrupt {
		case "magic":
			b[4] = 'X'
		case "kind":
			b[8] = 255
		case "generation":
			clear(b[10:18])
		case "secret-length":
			binary.BigEndian.PutUint32(b[:4], frameHeader+513)
		case "huge":
			binary.BigEndian.PutUint32(b[:4], ^uint32(0))
		}
		if _, err := ReadFrame(bytes.NewReader(b)); !errors.Is(err, ErrProtocol) {
			t.Fatalf("%s error %v", corrupt, err)
		}
	}
	f.Payload = append(f.Payload, 1)
	if err := WriteFrame(io.Discard, f); !errors.Is(err, ErrProtocol) {
		t.Fatal("oversize secret accepted")
	}
}

package auth

import (
	"encoding/binary"
	"io"

	"github.com/bnema/neferafk/internal/ports"
)

// The bootstrap record is the first and only non-frame traffic on the worker's
// stdin pipe (fd 3), written by the visual process once per lock:
//
//	"NAB1" | u32 length | generation u64 | 3 x (u32 length | bytes)
//
// where the byte strings are PINSource, PINReference and EnvPIN.
const bootstrapMax = 8 + 3*4 + 16 + ports.AuthMaxMetadata + ports.AuthMaxSecret

// WriteBootstrap encodes b; it wipes its encoded copy but not b.EnvPIN.
func WriteBootstrap(w io.Writer, b ports.AuthBootstrap) error {
	if b.Generation == 0 || len(b.PINSource) > 16 || len(b.PINReference) > ports.AuthMaxMetadata || len(b.EnvPIN) > ports.AuthMaxSecret {
		return ErrProtocol
	}
	p := make([]byte, 8, bootstrapMax+8)
	binary.BigEndian.PutUint64(p, b.Generation)
	for _, f := range [][]byte{[]byte(b.PINSource), []byte(b.PINReference), b.EnvPIN} {
		p = binary.BigEndian.AppendUint32(p, uint32(len(f)))
		p = append(p, f...)
	}
	out := append(append(make([]byte, 0, 8+len(p)), "NAB1"...), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(out[4:], uint32(len(p)))
	out = append(out, p...)
	clear(p)
	defer clear(out)
	return writeAll(w, out)
}

// ReadBootstrap decodes one record. The caller owns and wipes EnvPIN.
func ReadBootstrap(r io.Reader) (ports.AuthBootstrap, error) {
	var h [8]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return ports.AuthBootstrap{}, err
	}
	n := binary.BigEndian.Uint32(h[4:])
	if string(h[:4]) != "NAB1" || n < 8+12 || n > bootstrapMax {
		return ports.AuthBootstrap{}, ErrProtocol
	}
	p := make([]byte, n)
	defer clear(p)
	if _, err := io.ReadFull(r, p); err != nil {
		return ports.AuthBootstrap{}, err
	}
	b := ports.AuthBootstrap{Generation: binary.BigEndian.Uint64(p)}
	rest := p[8:]
	var f [3][]byte
	for i := range f {
		if len(rest) < 4 {
			return ports.AuthBootstrap{}, ErrProtocol
		}
		l := binary.BigEndian.Uint32(rest)
		if uint64(l) > uint64(len(rest)-4) {
			return ports.AuthBootstrap{}, ErrProtocol
		}
		f[i], rest = rest[4:4+l], rest[4+l:]
	}
	if b.Generation == 0 || len(rest) != 0 || len(f[0]) > 16 || len(f[1]) > ports.AuthMaxMetadata || len(f[2]) > ports.AuthMaxSecret {
		return ports.AuthBootstrap{}, ErrProtocol
	}
	b.PINSource, b.PINReference = ports.PINSource(f[0]), string(f[1])
	if len(f[2]) > 0 {
		b.EnvPIN = append([]byte(nil), f[2]...)
	}
	return b, nil
}

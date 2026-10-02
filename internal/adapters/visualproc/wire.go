// Package visualproc launches the separate visual process and speaks the tiny
// private daemon<->visual pipe protocol. It never renders and never owns or
// releases the session lock; the visual does, and reports the outcome.
//
// Wire format, both directions (fd 3: daemon->visual, fd 4: visual->daemon):
//
//	"NAV2" | kind u8 | length u32 BE | payload
//
// The magic names the format version: the daemon re-executes its binary as
// the visual, so an upgrade under a running daemon fails with ErrProtocol.
//
// Commands: 1 Fade {black u8, duration i64 ns BE}; 2 Lock {generation u64,
// then four u32-length-prefixed byte strings: PINSource, PINReference,
// EnvPIN, Output}. Events: 3 LockConfirmed {generation u64}; 4 LockReleased
// {generation u64}. Process exit is observed by the launcher, not sent.
package visualproc

import (
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/bnema/neferafk/internal/ports"
)

var ErrProtocol = errors.New("visualproc: invalid frame")

const (
	kindFade byte = iota + 1
	kindLock
	kindConfirmed
	kindReleased
	maxPayload = 8 + 4*4 + 2*ports.AuthMaxMetadata + ports.AuthMaxSecret + ports.MaxOutputName
)

const magic = "NAV2"

func writeFrame(w io.Writer, kind byte, payload []byte) error {
	buf := make([]byte, 9+len(payload))
	defer clear(buf)
	copy(buf, magic)
	buf[4] = kind
	binary.BigEndian.PutUint32(buf[5:], uint32(len(payload)))
	copy(buf[9:], payload)
	for b := buf; len(b) > 0; {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var h [9]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[5:])
	if string(h[:4]) != magic || n > maxPayload {
		return 0, nil, ErrProtocol
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		clear(p)
		return 0, nil, err
	}
	return h[4], p, nil
}

// WriteCommand encodes a daemon->visual command. The caller wipes any secret
// it owns; the encoded copy is wiped here.
func WriteCommand(w io.Writer, cmd ports.VisualCommand) error {
	switch c := cmd.(type) {
	case ports.VisualFade:
		p := make([]byte, 9)
		if c.Black {
			p[0] = 1
		}
		binary.BigEndian.PutUint64(p[1:], uint64(c.Duration))
		return writeFrame(w, kindFade, p)
	case ports.VisualLock:
		a := c.Auth
		fields := [][]byte{[]byte(a.PINSource), []byte(a.PINReference), a.EnvPIN, []byte(c.Output)}
		if c.Generation == 0 || uint64(c.Generation) != a.Generation || len(fields[1]) > ports.AuthMaxMetadata || len(fields[2]) > ports.AuthMaxSecret || len(fields[0]) > 16 || len(fields[3]) > ports.MaxOutputName {
			return ErrProtocol
		}
		p := binary.BigEndian.AppendUint64(nil, uint64(c.Generation))
		for _, f := range fields {
			p = binary.BigEndian.AppendUint32(p, uint32(len(f)))
			p = append(p, f...)
		}
		defer clear(p)
		return writeFrame(w, kindLock, p)
	}
	return ErrProtocol
}

// ReadCommand decodes a daemon->visual command (used by the visual process).
// The returned VisualLock owns EnvPIN and its receiver must wipe it.
func ReadCommand(r io.Reader) (ports.VisualCommand, error) {
	kind, p, err := readFrame(r)
	if err != nil {
		return nil, err
	}
	defer clear(p)
	switch kind {
	case kindFade:
		if len(p) != 9 || p[0] > 1 {
			return nil, ErrProtocol
		}
		d := time.Duration(binary.BigEndian.Uint64(p[1:]))
		if d < 0 || d > ports.MaxDuration {
			return nil, ErrProtocol
		}
		return ports.VisualFade{Black: p[0] == 1, Duration: d}, nil
	case kindLock:
		if len(p) < 8 {
			return nil, ErrProtocol
		}
		gen := binary.BigEndian.Uint64(p)
		rest := p[8:]
		var f [4][]byte
		for i := range f {
			if len(rest) < 4 {
				return nil, ErrProtocol
			}
			n := binary.BigEndian.Uint32(rest)
			if uint64(n) > uint64(len(rest)-4) {
				return nil, ErrProtocol
			}
			f[i] = rest[4 : 4+n]
			rest = rest[4+n:]
		}
		if gen == 0 || len(rest) != 0 || len(f[0]) > 16 || len(f[1]) > ports.AuthMaxMetadata || len(f[2]) > ports.AuthMaxSecret || len(f[3]) > ports.MaxOutputName {
			return nil, ErrProtocol
		}
		return ports.VisualLock{Generation: ports.Generation(gen), Auth: ports.AuthBootstrap{Generation: gen, PINSource: ports.PINSource(f[0]), PINReference: string(f[1]), EnvPIN: append([]byte(nil), f[2]...)}, Output: string(f[3])}, nil
	}
	return nil, ErrProtocol
}

// WriteEvent encodes a visual->daemon lifecycle event (used by the visual).
func WriteEvent(w io.Writer, ev ports.VisualEvent) error {
	switch e := ev.(type) {
	case ports.LockConfirmed:
		return writeFrame(w, kindConfirmed, binary.BigEndian.AppendUint64(nil, uint64(e.Generation)))
	case ports.LockReleased:
		return writeFrame(w, kindReleased, binary.BigEndian.AppendUint64(nil, uint64(e.Generation)))
	}
	return ErrProtocol
}

// ReadEvent decodes a visual->daemon event.
func ReadEvent(r io.Reader) (ports.VisualEvent, error) {
	kind, p, err := readFrame(r)
	if err != nil {
		return nil, err
	}
	if len(p) != 8 || binary.BigEndian.Uint64(p) == 0 {
		return nil, ErrProtocol
	}
	gen := ports.Generation(binary.BigEndian.Uint64(p))
	switch kind {
	case kindConfirmed:
		return ports.LockConfirmed{Generation: gen}, nil
	case kindReleased:
		return ports.LockReleased{Generation: gen}, nil
	}
	return nil, ErrProtocol
}

package auth

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/bnema/neferafk/internal/ports"
)

var ErrProtocol = errors.New("invalid authentication frame")

const frameHeader = 22 // magic, kind, generic code, generation, attempt

func payloadLimit(kind ports.AuthKind) int {
	if kind == ports.AuthAttempt || kind == ports.AuthSecret {
		return ports.AuthMaxSecret
	}
	return ports.AuthMaxMetadata
}

func validRole(f ports.AuthFrame, n int) bool {
	if f.Generation == 0 || n < 0 || n > payloadLimit(f.Kind) {
		return false
	}
	if f.Kind == ports.AuthReady {
		if f.Attempt != 0 {
			return false
		}
		switch f.Code {
		case ports.AuthPassword, ports.AuthPIN, ports.AuthFallbackPassword:
			return n == 0
		case ports.AuthUnavailable:
			return n == 0 || n == 1
		default:
			return false
		}
	}
	if f.Attempt == 0 {
		return false
	}
	switch f.Kind {
	case ports.AuthAttempt, ports.AuthSecret:
		return f.Code == 0
	case ports.AuthPrompt, ports.AuthStatus:
		return f.Code == ports.AuthPassword && n == 0
	case ports.AuthResult:
		return n == 0 && (f.Code == ports.AuthSuccess || f.Code == ports.AuthDenied || f.Code == ports.AuthUnavailable || f.Code == ports.AuthInvalid)
	case ports.AuthCancel:
		return f.Code == ports.AuthInvalid && n == 0
	default:
		return false
	}
}

func validFrame(f ports.AuthFrame) bool {
	if !validRole(f, len(f.Payload)) {
		return false
	}
	return len(f.Payload) != 1 || f.Kind != ports.AuthReady || f.Payload[0] == byte(ports.AuthFallbackToPassword)
}

// ReadFrame validates the fixed header before allocating any bounded payload.
// Partial reads wipe the allocated payload; no error includes input bytes.
func ReadFrame(r io.Reader) (ports.AuthFrame, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return ports.AuthFrame{}, err
	}
	n := int(binary.BigEndian.Uint32(prefix[:]))
	if n < frameHeader || n > frameHeader+ports.AuthMaxMetadata {
		return ports.AuthFrame{}, ErrProtocol
	}
	var h [frameHeader]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return ports.AuthFrame{}, err
	}
	f := ports.AuthFrame{Kind: ports.AuthKind(h[4]), Code: ports.AuthCode(h[5]), Generation: binary.BigEndian.Uint64(h[6:14]), Attempt: binary.BigEndian.Uint64(h[14:22])}
	if string(h[:4]) != "NAW1" || !validRole(f, n-frameHeader) {
		return ports.AuthFrame{}, ErrProtocol
	}
	f.Payload = make([]byte, n-frameHeader)
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		clear(f.Payload)
		return ports.AuthFrame{}, err
	}
	if !validFrame(f) {
		clear(f.Payload)
		return ports.AuthFrame{}, ErrProtocol
	}
	return f, nil
}

func WriteFrame(w io.Writer, f ports.AuthFrame) error {
	if !validFrame(f) {
		return ErrProtocol
	}
	var h [4 + frameHeader]byte
	binary.BigEndian.PutUint32(h[:4], uint32(frameHeader+len(f.Payload)))
	copy(h[4:8], "NAW1")
	h[8], h[9] = byte(f.Kind), byte(f.Code)
	binary.BigEndian.PutUint64(h[10:18], f.Generation)
	binary.BigEndian.PutUint64(h[18:26], f.Attempt)
	if err := writeAll(w, h[:]); err != nil {
		return err
	}
	return writeAll(w, f.Payload)
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) != 0 {
		n, err := w.Write(b)
		if n < 0 || n > len(b) {
			return ErrProtocol
		}
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

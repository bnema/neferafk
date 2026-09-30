package auth

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	pam "github.com/bnema/purego-pam"
)

func TestFallbackReadyIsExplicitEvenWithoutPAM(t *testing.T) {
	for _, available := range []bool{false, true} {
		var output bytes.Buffer
		var backend pamBackend
		if available {
			backend = newMockpamBackend(t)
		}
		if err := runScoped(bytes.NewReader(nil), &output, 3, nil, backend, "user", true); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		f, err := ReadFrame(&output)
		if err != nil {
			t.Fatal(err)
		}
		if available {
			if f.Code != ports.AuthFallbackPassword || len(f.Payload) != 0 {
				t.Fatalf("fallback ready %+v", f)
			}
		} else if f.Code != ports.AuthUnavailable || !bytes.Equal(f.Payload, []byte{byte(ports.AuthFallbackToPassword)}) {
			t.Fatalf("fallback unavailable %+v", f)
		}
	}
}

func TestEchoOnNeverReceivesPresubmittedCredential(t *testing.T) {
	var input, output bytes.Buffer
	if err := WriteFrame(&input, ports.AuthFrame{Kind: ports.AuthSecret, Generation: 3, Attempt: 9, Payload: []byte("explicit-response")}); err != nil {
		t.Fatal(err)
	}
	w := &worker{r: &input, w: &output, generation: 3, attempt: 9, deadline: time.Now().Add(time.Second), first: true, secretLen: 8, secret: make([]byte, 512)}
	copy(w.secret, "password")
	answer := make([]byte, 512)
	n, err := w.Respond(pam.Message{Style: pam.PromptEchoOn}, answer)
	if err != nil || string(answer[:n]) != "explicit-response" || !w.first {
		t.Fatal("echo-on leaked or consumed initial password")
	}
	clear(answer)
	n, err = w.Respond(pam.Message{Style: pam.PromptEchoOff}, answer)
	if err != nil || string(answer[:n]) != "password" || w.first || !bytes.Equal(w.secret, make([]byte, 512)) {
		t.Fatal("echo-off did not consume and wipe initial credential")
	}
	f, err := ReadFrame(&output)
	if err != nil || f.Kind != ports.AuthPrompt || len(f.Payload) != 0 {
		t.Fatal("echo-on did not emit generic prompt")
	}
}

func TestCodecRejectsContradictoryRoles(t *testing.T) {
	frames := []ports.AuthFrame{
		{Kind: ports.AuthReady, Generation: 1},
		{Kind: ports.AuthResult, Generation: 1, Attempt: 1},
		{Kind: ports.AuthStatus, Generation: 1, Attempt: 1},
		{Kind: ports.AuthResult, Code: ports.AuthPIN, Generation: 1, Attempt: 1},
		{Kind: ports.AuthResult, Code: ports.AuthSuccess, Generation: 1, Attempt: 1, Payload: []byte("raw")},
		{Kind: ports.AuthReady, Code: ports.AuthUnavailable, Generation: 1, Payload: []byte{99}},
		{Kind: ports.AuthReady, Code: ports.AuthPassword, Generation: 1, Attempt: 1},
		{Kind: ports.AuthAttempt, Code: ports.AuthSuccess, Generation: 1, Attempt: 1},
		{Kind: ports.AuthSecret, Generation: 1},
	}
	for _, f := range frames {
		if err := WriteFrame(io.Discard, f); !errors.Is(err, ErrProtocol) {
			t.Fatalf("contradictory role accepted: kind=%d code=%d", f.Kind, f.Code)
		}
	}
}

func TestStaleGenerationSecretNeverProducesResult(t *testing.T) {
	var input, output bytes.Buffer
	f := ports.AuthFrame{Kind: ports.AuthAttempt, Generation: 2, Attempt: 1, Payload: []byte("synthetic")}
	if err := WriteFrame(&input, f); err != nil {
		t.Fatal(err)
	}
	if err := run(&input, &output, 3, nil, nil, ""); !errors.Is(err, ErrProtocol) {
		t.Fatalf("stale generation accepted: %v", err)
	}
	ready, err := ReadFrame(&output)
	if err != nil || ready.Kind != ports.AuthReady {
		t.Fatal("missing readiness")
	}
	if _, err := ReadFrame(&output); !errors.Is(err, io.EOF) {
		t.Fatal("stale secret produced result")
	}
}

func TestDedicatedSecretPageCapacityAndRetirement(t *testing.T) {
	for _, size := range []int{32, 512} {
		b, err := newSecret(size)
		if err != nil {
			t.Fatal(err)
		}
		if len(b.data) != size || cap(b.data) != size || len(b.page) < size {
			t.Fatal("secret capacity not bounded")
		}
		copy(b.data, "synthetic")
		b.close()
		if b.data != nil || b.page != nil {
			t.Fatal("retired native memory still accessible")
		}
		b.close()
	}
}

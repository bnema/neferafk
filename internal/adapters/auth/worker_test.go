package auth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	pam "github.com/bnema/purego-pam"
	"github.com/stretchr/testify/mock"
)

// Unlock runs only the PAM auth stack (like swaylock); account management is
// never called, so a service file with only `auth` lines works.
func TestPAMRequiresAuthenticationAndEnd(t *testing.T) {
	for _, stage := range []string{"success", "start", "authenticate", "end"} {
		t.Run(stage, func(t *testing.T) {
			b := newMockpamBackend(t)
			tx := newMockpamTransaction(t)
			boom := errors.New("sensitive native message")
			if stage == "start" {
				b.EXPECT().start("neferafk", "real-user", mock.Anything).Return(nil, boom).Once()
			} else {
				b.EXPECT().start("neferafk", "real-user", mock.Anything).Return(tx, nil).Once()
				var authErr, endErr error
				if stage == "authenticate" {
					authErr = boom
				}
				if stage == "end" {
					endErr = boom
				}
				tx.EXPECT().Authenticate(pam.DisallowNullAuthtok).Return(authErr).Once()
				tx.EXPECT().End().Return(endErr).Once()
			}
			code := authenticate(b, "real-user", &worker{})
			if (code == ports.AuthSuccess) != (stage == "success") {
				t.Fatalf("stage %s code %d", stage, code)
			}
		})
	}
}

func TestPINWrongNeverFallsBackAndAttemptsScoped(t *testing.T) {
	var input, output bytes.Buffer
	for i, secret := range []string{"000000", "123456"} {
		if err := WriteFrame(&input, ports.AuthFrame{Kind: ports.AuthAttempt, Generation: 8, Attempt: uint64(i + 1), Payload: []byte(secret)}); err != nil {
			t.Fatal(err)
		}
	}
	b := newMockpamBackend(t) // no PAM call allowed for valid resolved PIN
	if err := run(&input, &output, 8, []byte("123456"), b, "user"); err != nil {
		t.Fatal(err)
	}
	for i, want := range []ports.AuthCode{ports.AuthPIN, ports.AuthDenied, ports.AuthSuccess} {
		f, err := ReadFrame(&output)
		if err != nil || f.Code != want || f.Generation != 8 || f.Attempt != uint64(i) || len(f.Payload) != 0 {
			t.Fatalf("result %+v err=%v", f, err)
		}
	}
}

func TestRunConsumesBootstrapOnceAndWipes(t *testing.T) {
	var input, output bytes.Buffer
	if err := WriteFrame(&input, ports.AuthFrame{Kind: ports.AuthAttempt, Generation: 8, Attempt: 1, Payload: []byte("123456")}); err != nil {
		t.Fatal(err)
	}
	bootstrap := ports.AuthBootstrap{Generation: 8, PINSource: "env", EnvPIN: []byte("123456")}
	if err := Run(context.Background(), &input, &output, bootstrap); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bootstrap.EnvPIN, make([]byte, 6)) {
		t.Fatal("bootstrap retained secret")
	}
	for _, kind := range []ports.AuthKind{ports.AuthReady, ports.AuthResult} {
		f, err := ReadFrame(&output)
		if err != nil || f.Kind != kind {
			t.Fatalf("frame %+v %v", f, err)
		}
	}
}

func TestUnavailablePAMAndReplayFailClosed(t *testing.T) {
	var input, output bytes.Buffer
	for range 2 {
		if err := WriteFrame(&input, ports.AuthFrame{Kind: ports.AuthAttempt, Generation: 2, Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(&input, &output, 2, nil, nil, ""); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	for range 2 {
		f, err := ReadFrame(&output)
		if err != nil || f.Code != ports.AuthUnavailable {
			t.Fatalf("result %+v %v", f, err)
		}
	}
	if _, err := ReadFrame(&output); !errors.Is(err, io.EOF) {
		t.Fatal("replay produced result")
	}
}

func TestConversationGenericMultiplePromptsAndWipes(t *testing.T) {
	var input, output bytes.Buffer
	if err := WriteFrame(&input, ports.AuthFrame{Kind: ports.AuthSecret, Generation: 3, Attempt: 9, Payload: []byte("second")}); err != nil {
		t.Fatal(err)
	}
	w := &worker{r: &input, w: &output, generation: 3, attempt: 9, deadline: time.Now().Add(time.Second), first: true, secretLen: 5, secret: make([]byte, 512)}
	copy(w.secret[:], "first")
	answer := make([]byte, 512)
	if n, err := w.Respond(pam.Message{Style: pam.PromptEchoOff, Text: []byte("do not forward")}, answer); err != nil || string(answer[:n]) != "first" {
		t.Fatalf("first %d %v", n, err)
	}
	if !bytes.Equal(w.secret[:], make([]byte, 512)) {
		t.Fatal("first secret retained")
	}
	clear(answer)
	if n, err := w.Respond(pam.Message{Style: pam.PromptEchoOn, Text: []byte("raw native prompt")}, answer); err != nil || string(answer[:n]) != "second" {
		t.Fatalf("second %d %v", n, err)
	}
	if _, err := w.Respond(pam.Message{Style: pam.ErrorMsg, Text: []byte("native secret error")}, answer); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []ports.AuthKind{ports.AuthPrompt, ports.AuthStatus} {
		f, err := ReadFrame(&output)
		if err != nil || f.Kind != kind || len(f.Payload) != 0 || f.Attempt != 9 {
			t.Fatalf("generic %+v %v", f, err)
		}
	}
}

package auth

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/bnema/neferafk/internal/ports"
)

func TestPINSourceValidation(t *testing.T) {
	for _, input := range []string{"12345", "123456\n", "12345a", " 123456", "123456 ", "", "123456\r", "123456789012345678901234567890123"} {
		if pin := ResolvePIN(context.Background(), ports.AuthBootstrap{PINSource: "env", EnvPIN: []byte(input)}); pin != nil {
			t.Fatal("invalid env source accepted")
		}
	}
	in := []byte("123456")
	pin := ResolvePIN(context.Background(), ports.AuthBootstrap{PINSource: "env", EnvPIN: in})
	if !bytes.Equal(pin, in) {
		t.Fatal("valid env source rejected")
	}
	clear(in)
	if string(pin) != "123456" {
		t.Fatal("resolved PIN aliases bootstrap")
	}
	clear(pin)
	for _, entry := range []string{"", "-clip", "/absolute", "a/../b", "a//b", "a/./b", "a\\b", "a\x00b"} {
		if passEntry(entry) {
			t.Fatal("unsafe pass entry accepted")
		}
	}
	if !passEntry("locker/pin") {
		t.Fatal("valid pass reference rejected")
	}
	var out fixedOutput
	if _, err := out.Write(make([]byte, sourceLimit)); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write([]byte{1}); err == nil || out.n != sourceLimit {
		t.Fatal("source output cap not enforced")
	}
	clear(out.data[:])
}

func TestSourceGenericChildBoundsAndTimeout(t *testing.T) {
	pin, err := commandPIN(context.Background(), "/bin/sh", []string{"-c", "printf '123456\\nmetadata\\n'"}, []string{"PATH=/usr/bin:/bin"})
	if err != nil || string(pin) != "123456" {
		t.Fatalf("first line source: %v", err)
	}
	clear(pin)
	if _, err := commandPIN(context.Background(), "/bin/sh", []string{"-c", "head -c 5000 /dev/zero"}, []string{"PATH=/usr/bin:/bin"}); err == nil {
		t.Fatal("oversize source accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := commandPIN(ctx, "/bin/sh", []string{"-c", "sleep 10 & wait"}, []string{"PATH=/usr/bin:/bin"}); err == nil {
		t.Fatal("timed-out source accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("source group cancellation did not bound Wait")
	}
}

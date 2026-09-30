package config

import (
	"github.com/bnema/neferafk/internal/ports"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAndIndependentOff(t *testing.T) {
	c, err := Parse(strings.NewReader("# defaults\n"))
	if err != nil || c != ports.Defaults() {
		t.Fatal(c, err)
	}
	c, err = Parse(strings.NewReader("lock.after = off\nfade.after = 1s\nfade.duration = 0s\nscreens.off-after = off\nsleep.after = 24h\nauth.mode = pin\nauth.pin-source = env\nauth.pin-env = NEFERAFK_PIN\n"))
	if err != nil || c.LockAfter != 0 || c.FadeAfter != time.Second || c.FadeDuration != 0 || c.SleepAfter != 24*time.Hour || c.Auth.PINEnv != "NEFERAFK_PIN" {
		t.Fatal(c, err)
	}
}
func TestRejectTransactionalBoundsUnknownDuplicateAndSecrets(t *testing.T) {
	for _, text := range []string{"unknown = x", "lock.after=1s\nlock.after=2s", "lock.after=0s", "lock.after=-1s", "sleep.after=25h", "fade.duration=-1s", "fade.duration=off", "auth.password=false", "auth.pin-reference=x", "auth.mode=unknown", "auth.mode=pin", "auth.pin-source=pass\nauth.pin-entry=-option", "auth.pin-source=pass\nauth.pin-entry=a\x00b", "auth.pin=1234", "auth.pin-source=env\nauth.pin-env=SECRET=123", "auth.pin-source=pass\nauth.pin-entry=../secret", "auth.pin-source=pass\nauth.pin-entry=/secret", strings.Repeat("#", MaxLine+1), strings.Repeat("#\n", MaxBytes)} {
		c, err := Parse(strings.NewReader(text))
		if err == nil || c != (ports.Config{}) {
			t.Fatalf("accepted/partial invalid input length %d", len(text))
		}
	}
}
func TestPassReferenceMetadataOnly(t *testing.T) {
	c, err := Parse(strings.NewReader("auth.pin-source=pass\nauth.pin-entry=desktop/neferafk\n"))
	if err != nil || c.Auth.PINEntry != "desktop/neferafk" {
		t.Fatal(c, err)
	}
}
func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/config")
	if got := DefaultPath(); got != "/config/neferafk/config" {
		t.Fatal(got)
	}
}

func TestUnknownKeyDiagnosticCannotEchoSecret(t *testing.T) {
	secret := "accidental-password-should-never-appear"
	_, err := Parse(strings.NewReader(secret + " = " + secret))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe diagnostic %v", err)
	}
}
func TestRejectInvalidUTF8BeforeParsingAndDirectMetadata(t *testing.T) {
	for _, text := range []string{"# \xff", "auth.pin-source=pass\nauth.pin-entry=a\xff"} {
		if _, err := Parse(strings.NewReader(text)); err == nil {
			t.Fatal("invalid UTF-8 parsed")
		}
	}
	cfg := ports.Defaults()
	cfg.Auth.PINSource = ports.PINSourcePass
	cfg.Auth.PINEntry = "entry\xff"
	if err := cfg.Validate(); err == nil {
		t.Fatal("direct invalid UTF-8 metadata admitted")
	}
}

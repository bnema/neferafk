package config

import (
	"bnema.dev/neferafk/internal/ports"
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
func TestLockOutput(t *testing.T) {
	c, err := Parse(strings.NewReader("lock.output = DP-2\n"))
	if err != nil || c.LockOutput != "DP-2" {
		t.Fatal(c, err)
	}
	if _, err := Parse(strings.NewReader("lock.output = " + strings.Repeat("x", ports.MaxOutputName+1))); err == nil {
		t.Fatal("oversized output name accepted")
	}
	cfg := ports.Defaults()
	cfg.LockOutput = "DP\x01"
	if err := cfg.Validate(); err == nil {
		t.Fatal("control character in output name accepted")
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

func TestParseReportsFirstProblemInLineOrder(t *testing.T) {
	long := strings.Repeat("#", MaxLine+1)
	for _, tc := range []struct{ name, text, want string }{
		{"bad value before missing =", "lock.after=bad\nfoo", "line 1: "},
		{"bad value before duplicate", "lock.after=bad\nlock.after=1s", "line 1: "},
		{"unknown key before missing =", "unknown=x\nnoeq", "line 1: unknown key"},
		{"bad value before long line", "lock.after=bad\n" + long, "line 1: "},
		{"missing = before bad value", "noeq\nlock.after=bad", "line 1: expected key = value"},
		{"duplicate before later bad value", "lock.after=1s\nlock.after=2s\nsleep.after=bad", "line 2: overrides line 1"},
		{"duplicate wins tie with its bad value", "lock.after=1s\nlock.after=bad", "line 2: overrides line 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse(strings.NewReader(tc.text))
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || c != (ports.Config{}) {
				t.Fatalf("got %v, %+v; want error starting %q", err, c, tc.want)
			}
		})
	}
}

func TestParseHashStartsCommentEvenInQuotes(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"plain", "lock.output = DP-2 # left", "DP-2"},
		{"no space", "lock.output = DP-2#left", "DP-2"},
		{"inside double quotes", `lock.output = "DP#2"`, `"DP`},
		{"after unbalanced quote", `lock.output = DP-2 "# note`, `DP-2 "`},
		{"inside single quotes", "lock.output = 'DP#2'", "'DP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse(strings.NewReader(tc.text))
			if err != nil || c.LockOutput != tc.want {
				t.Fatalf("got %q, %v; want %q", c.LockOutput, err, tc.want)
			}
		})
	}
	if _, err := Parse(strings.NewReader("lock.output = # only a comment")); err == nil {
		t.Fatal("value made empty by a comment accepted")
	}
}

// The kv tags on ports.Config repeat MaxDuration and the auth constants.
func TestTagsMatchPortsConstants(t *testing.T) {
	max := ports.MaxDuration.String()
	for _, key := range []string{"lock.after", "fade.after", "screens.off-after", "sleep.after", "fade.duration"} {
		if _, err := Parse(strings.NewReader(key + "=" + max)); err != nil {
			t.Errorf("%s=%s rejected: %v", key, max, err)
		}
		if _, err := Parse(strings.NewReader(key + "=" + (ports.MaxDuration + 1).String())); err == nil {
			t.Errorf("%s above %s accepted", key, max)
		}
	}
	for _, text := range []string{
		"auth.mode=" + string(ports.AuthModePassword),
		"auth.mode=" + string(ports.AuthModePIN) + "\nauth.pin-source=" + string(ports.PINSourceEnv) + "\nauth.pin-env=PIN",
		"auth.pin-source=" + string(ports.PINSourcePass) + "\nauth.pin-entry=a/b",
	} {
		if _, err := Parse(strings.NewReader(text)); err != nil {
			t.Errorf("%q rejected: %v", text, err)
		}
	}
}

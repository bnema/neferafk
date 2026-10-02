// Package config reads bounded transactional flat key=value configuration.
package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bnema/neferafk/internal/ports"
)

const MaxBytes = 64 << 10
const MaxLine = 4 << 10

func DefaultPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".config")
		}
	}
	return filepath.Join(base, "neferafk", "config")
}
func Load(path string) (ports.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return ports.Config{}, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse never returns partial configuration and never resolves PIN references.
func Parse(r io.Reader) (ports.Config, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return ports.Config{}, err
	}
	if !utf8.Valid(data) {
		return ports.Config{}, fmt.Errorf("config is not valid UTF-8")
	}
	if len(data) > MaxBytes {
		return ports.Config{}, fmt.Errorf("config exceeds 64KiB")
	}
	cfg := ports.Defaults()
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, MaxLine+1), MaxLine+2)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if len(text) > MaxLine {
			return ports.Config{}, fmt.Errorf("line %d exceeds 4KiB", line)
		}
		text = strings.TrimSpace(strings.SplitN(text, "#", 2)[0])
		if text == "" {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || value == "" || seen[key] {
			return ports.Config{}, fmt.Errorf("line %d: missing value or duplicate key", line)
		}
		seen[key] = true
		var target *time.Duration
		switch key {
		case "lock.after":
			target = &cfg.LockAfter
		case "fade.after":
			target = &cfg.FadeAfter
		case "fade.duration":
			target = &cfg.FadeDuration
		case "screens.off-after":
			target = &cfg.OffAfter
		case "sleep.after":
			target = &cfg.SleepAfter
		case "lock.output":
			cfg.LockOutput = value
		case "auth.mode":
			cfg.Auth.Mode = ports.AuthMode(value)
		case "auth.pin-source":
			cfg.Auth.PINSource = ports.PINSource(value)
		case "auth.pin-env":
			cfg.Auth.PINEnv = value
		case "auth.pin-entry":
			cfg.Auth.PINEntry = value
		default:
			return ports.Config{}, fmt.Errorf("line %d: unknown key", line)
		}
		if target != nil {
			if value == "off" && key != "fade.duration" {
				*target = 0
				continue
			}
			d, e := time.ParseDuration(value)
			if e != nil || d < 0 || d > ports.MaxDuration || d == 0 && key != "fade.duration" {
				return ports.Config{}, fmt.Errorf("line %d: invalid duration", line)
			}
			*target = d
		}
	}
	if err := scanner.Err(); err != nil {
		return ports.Config{}, fmt.Errorf("config line exceeds bound")
	}
	if err := cfg.Validate(); err != nil {
		return ports.Config{}, err
	}
	return cfg, nil
}

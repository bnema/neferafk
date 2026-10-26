// Package config reads bounded transactional flat key=value configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"bnema.dev/kvconf"
	"bnema.dev/neferafk/internal/ports"
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
	data, err := kvconf.ReadFile(path, options)
	if err != nil {
		if errors.Is(err, kvconf.ErrTooLarge) {
			return ports.Config{}, errTooLarge
		}
		return ports.Config{}, err
	}
	return parse(data)
}

// Parse never returns partial configuration and never resolves PIN references.
func Parse(r io.Reader) (ports.Config, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return ports.Config{}, err
	}
	return parse(data)
}

// options: values are never unquoted, and parse cuts each one at its first
// "#", even inside quotes. parse rejects the whole file on any problem.
var options = kvconf.Options{Comments: kvconf.CommentLine, RawValues: true, MaxBytes: MaxBytes, MaxLine: MaxLine}

var errTooLarge = errors.New("config exceeds 64KiB")

// parse fails on the first problem in line order. Strict mode would report
// a line without "=" or a duplicate before an earlier bad value, so the file
// is decoded leniently and its problems compared; a syntax problem wins a tie.
func parse(data []byte) (ports.Config, error) {
	entries, syntax, err := kvconf.ParseBytes(data, options)
	if err != nil {
		return ports.Config{}, reject(err)
	}
	for i := range entries {
		if v, _, cut := strings.Cut(entries[i].Value, "#"); cut {
			entries[i].Value = strings.TrimSpace(v)
		}
	}
	cfg := ports.Defaults()
	values, err := kvconf.Decode(entries, &cfg, options)
	switch {
	case len(syntax) > 0 && (len(values) == 0 || syntax[0].Line <= values[0].Line):
		return ports.Config{}, lineError(syntax[0])
	case len(values) > 0:
		return ports.Config{}, lineError(values[0])
	case err != nil:
		return ports.Config{}, err
	}
	return cfg, nil
}

func reject(err error) error {
	switch {
	case errors.Is(err, kvconf.ErrTooLarge):
		return errTooLarge
	case errors.Is(err, kvconf.ErrNotUTF8):
		return errors.New("config is not valid UTF-8")
	}
	return err
}

// lineError carries a line number and a fixed message, never the key or
// value, which may be secret.
func lineError(w kvconf.Warning) error {
	return fmt.Errorf("line %d: %s", w.Line, w.Msg)
}

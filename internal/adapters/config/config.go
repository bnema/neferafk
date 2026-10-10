// Package config reads bounded transactional flat key=value configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bnema/kvconf"
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

// options: any problem rejects the whole file; "#" starts a comment anywhere
// and values are never unquoted.
var options = kvconf.Options{Strict: true, Comments: kvconf.CommentAny, RawValues: true, MaxBytes: MaxBytes, MaxLine: MaxLine}

var errTooLarge = errors.New("config exceeds 64KiB")

func parse(data []byte) (ports.Config, error) {
	cfg := ports.Defaults()
	if _, err := kvconf.Unmarshal(data, &cfg, options); err != nil {
		return ports.Config{}, reject(err)
	}
	return cfg, nil
}

// reject rewrites kvconf errors so they carry a line number and a fixed
// message, never a key or value, which may be secret.
func reject(err error) error {
	var w kvconf.Warning
	switch {
	case errors.Is(err, kvconf.ErrTooLarge):
		return errTooLarge
	case errors.Is(err, kvconf.ErrNotUTF8):
		return errors.New("config is not valid UTF-8")
	case errors.As(err, &w):
		return fmt.Errorf("line %d: %s", w.Line, w.Msg)
	}
	return err
}

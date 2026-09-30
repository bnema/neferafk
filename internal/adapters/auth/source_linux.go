package auth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/bnema/neferafk/internal/ports"
)

const sourceLimit = 4096

var errSource = errors.New("PIN source unavailable")

func validPIN(b []byte) bool {
	if len(b) < 6 || len(b) > 32 {
		return false
	}
	for _, v := range b {
		if v < '0' || v > '9' {
			return false
		}
	}
	return true
}

func passEntry(entry string) bool {
	if entry == "" || len(entry) > ports.AuthMaxMetadata || strings.HasPrefix(entry, "-") || strings.HasPrefix(entry, "/") || strings.ContainsAny(entry, "\\\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(entry, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// fixedOutput bounds stdout without retaining raw errors or growing buffers.
type fixedOutput struct {
	data [sourceLimit]byte
	n    int
}

func (b *fixedOutput) Write(p []byte) (int, error) {
	if len(p) > len(b.data)-b.n {
		return 0, errSource
	}
	copy(b.data[b.n:], p)
	b.n += len(p)
	return len(p), nil
}

func passEnvironment() []string {
	var env []string
	for _, key := range []string{"HOME", "PATH", "GNUPGHOME", "PASSWORD_STORE_DIR"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	// Installed pass uses PASSWORD_STORE_GPG_OPTS; prevent TTY/pinentry and
	// extension execution. No startup PIN or typed secret enters this env.
	return append(env, "PASSWORD_STORE_GPG_OPTS=--batch --no-tty --pinentry-mode error", "PASSWORD_STORE_ENABLE_EXTENSIONS=false", "GPG_TTY=", "LC_ALL=C")
}

func readPass(ctx context.Context, entry string) ([]byte, error) {
	if !passEntry(entry) {
		return nil, errSource
	}
	return commandPIN(ctx, "pass", []string{"show", entry}, passEnvironment())
}

// commandPIN owns only the bounded source subprocess, never a shell command
// assembled from metadata. Tests use a generic child, not a real pass store.
func commandPIN(ctx context.Context, executable string, args, env []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Kill the complete source group, then Run's Wait reaps the child.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 100 * time.Millisecond
	var out fixedOutput
	cmd.Stdout = &out
	defer clear(out.data[:])
	if err := cmd.Run(); err != nil {
		return nil, errSource
	}
	line := out.data[:out.n]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if !validPIN(line) {
		return nil, errSource
	}
	return bytes.Clone(line), nil
}

// ResolvePIN is called once per lock, not once per attempt. The caller owns and
// must wipe the returned mutable slice. Invalid/unavailable source means PAM.
func ResolvePIN(ctx context.Context, bootstrap ports.AuthBootstrap) []byte {
	switch bootstrap.PINSource {
	case ports.PINSourceEnv:
		if validPIN(bootstrap.EnvPIN) {
			return bytes.Clone(bootstrap.EnvPIN)
		}
	case ports.PINSourcePass:
		pin, err := readPass(ctx, bootstrap.PINReference)
		if err == nil {
			return pin
		}
	}
	return nil
}

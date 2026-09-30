package visual

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// execLauncher spawns `<exe> auth-worker` with private pipes: child fd 3 is the
// worker's input, fd 4 its output. No credential ever appears in argv or env.
type execLauncher struct{ exe string }

type execWorker struct {
	cmd     *exec.Cmd
	in, out *os.File
}

func (l execLauncher) launch(context.Context) (workerProcess, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create auth worker input pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, fmt.Errorf("create auth worker output pipe: %w", err)
	}
	cmd := exec.Command(l.exe, "auth-worker")
	cmd.Stderr = os.Stderr
	cmd.Env = workerEnvironment(os.LookupEnv)
	cmd.ExtraFiles = []*os.File{inR, outW}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		return nil, fmt.Errorf("start auth worker %s: %w", l.exe, err)
	}
	return &execWorker{cmd: cmd, in: inW, out: outR}, nil
}

// workerEnvAllowlist is everything the worker (and the `pass`/gpg it spawns)
// may inherit from the visual process; nothing else leaks into it.
var workerEnvAllowlist = []string{
	"HOME", "PATH", "USER", "LOGNAME", "LANG", "LC_ALL",
	"XDG_RUNTIME_DIR", "GNUPGHOME", "PASSWORD_STORE_DIR", "GPG_TTY",
}

// workerEnvironment builds the worker's environment from the allowlist; only
// variables that are present are passed.
func workerEnvironment(lookup func(string) (string, bool)) []string {
	env := make([]string, 0, len(workerEnvAllowlist))
	for _, key := range workerEnvAllowlist {
		if v, ok := lookup(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

func (w *execWorker) pipes() (io.WriteCloser, io.ReadCloser) { return w.in, w.out }
func (w *execWorker) kill() error {
	return syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
}
func (w *execWorker) wait() error { return w.cmd.Wait() }

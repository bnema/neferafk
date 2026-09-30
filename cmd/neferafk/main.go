// Command neferafk is the NeferAFK absence daemon and its private tools.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"syscall"

	"github.com/bnema/neferafk/internal/adapters/auth"
	"github.com/bnema/neferafk/internal/adapters/config"
	"github.com/bnema/neferafk/internal/adapters/control"
	"github.com/bnema/neferafk/internal/adapters/visual"
	"github.com/bnema/neferafk/internal/app"
	"github.com/bnema/zerowrap"
)

const (
	exitOK          = 0
	exitFailure     = 1
	exitUsage       = 2
	exitUnsupported = 3
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: neferafk <command> [flags]

commands:
  run [--config FILE]   run the absence daemon
  lock                  ask the running daemon to lock now
  status                print the running daemon's state
  validate-config FILE  check a configuration file and exit
  version               print the version
  visual                internal: fade/lock process (fd 3 commands in, fd 4 events out)
`

func main() { os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr)) }

func execute(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run":
		return runDaemon(rest, stderr)
	case "lock", "status":
		return runControl(cmd, rest, stdout, stderr)
	case "visual":
		return runVisual(rest, os.NewFile(3, "visual-cmd"), os.NewFile(4, "visual-events"), stderr)
	case "auth-worker": // hidden: spawned by the visual process only
		return runAuthWorker(rest, os.NewFile(3, "auth-in"), os.NewFile(4, "auth-out"), stderr)
	case "version":
		if len(rest) != 0 {
			fmt.Fprint(stderr, usage)
			return exitUsage
		}
		fmt.Fprintln(stdout, version)
		return exitOK
	case "validate-config":
		if len(rest) != 1 {
			fmt.Fprint(stderr, usage)
			return exitUsage
		}
		if _, err := config.Load(rest[0]); err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				fmt.Fprintf(stderr, "neferafk: %v\n", err) // already names the file
			} else {
				fmt.Fprintf(stderr, "neferafk: %s: %v\n", rest[0], err)
			}
			return exitFailure
		}
		return exitOK
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "neferafk: unknown command %q\n%s", cmd, usage)
	return exitUsage
}

func runDaemon(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := fs.String("config", "", "configuration file (default $XDG_CONFIG_HOME/neferafk/config)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "neferafk run: unexpected arguments")
		return exitUsage
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "neferafk: %v\n", err)
		return exitFailure
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Compositors usually start the daemon with stderr discarded, so it also
	// logs to $XDG_STATE_HOME/neferafk/logs/daemon.log (never secrets).
	log, closeLog, err := zerowrap.NewWithFile(zerowrap.Config{Output: stderr},
		zerowrap.FileConfig{Enabled: true, AppName: "neferafk", Name: "daemon", MaxSize: 5, MaxBackups: 1})
	if err != nil {
		log = zerowrap.New(zerowrap.Config{Output: stderr})
		closeLog = func() {}
		log.Warn().Str("component", "main").Err(err).Msg("file logging disabled")
	}
	defer closeLog()
	err = app.RunDaemon(ctx, app.DaemonOptions{ConfigPath: *cfg, Executable: exe, Log: log})
	var unsup *app.UnsupportedError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &unsup):
		log.Error().Str("component", "main").Strs("missing", unsup.Missing).Msg("unsupported environment")
		fmt.Fprintf(stderr, "neferafk: %v\n", unsup)
		return exitUnsupported
	default:
		log.Error().Str("component", "main").Err(err).Msg("daemon failed")
		fmt.Fprintf(stderr, "neferafk: %v\n", err)
		return exitFailure
	}
}

func runControl(cmd string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "neferafk %s: unexpected arguments\n", cmd)
		return exitUsage
	}
	path, err := control.DefaultPath()
	if err != nil {
		fmt.Fprintf(stderr, "neferafk: %v\n", err)
		return exitFailure
	}
	reply, err := control.Do(context.Background(), path, cmd)
	if err != nil {
		fmt.Fprintf(stderr, "neferafk %s: %v\n", cmd, err)
		return exitFailure
	}
	fmt.Fprintln(stdout, reply)
	return exitOK
}

// Descriptors the daemon hands down by exec ExtraFiles: fd 3 in, fd 4 out.
// Go clears close-on-exec on them in the child, so anything we spawn later
// (`pass`, gpg) would inherit the secret input pipe and the result output pipe
// and could read secrets or forge results. Mark them before doing anything.
const (
	inheritedIn  = 3
	inheritedOut = 4
)

func markInheritedCloexec() {
	syscall.CloseOnExec(inheritedIn)
	syscall.CloseOnExec(inheritedOut)
}

// runAuthWorker serves one lock's authentication on the private pipes handed
// down by the visual process (fd 3: bootstrap + frames in, fd 4: frames out).
// Nothing typed ever passes through the policy daemon.
func runAuthWorker(args []string, in, out *os.File, stderr io.Writer) int {
	if len(args) != 0 || in == nil || out == nil {
		fmt.Fprintln(stderr, "neferafk auth-worker: internal command")
		return exitUsage
	}
	markInheritedCloexec() // before anything is read or spawned
	if err := auth.HardenProcess(); err != nil {
		fmt.Fprintln(stderr, "neferafk auth-worker: hardening failed")
		return exitFailure
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serveAuth(ctx, in, out, stderr)
}

// serveAuth reads the bootstrap record and runs the worker loop.
func serveAuth(ctx context.Context, in io.Reader, out io.Writer, stderr io.Writer) int {
	bootstrap, err := auth.ReadBootstrap(in)
	if err != nil {
		fmt.Fprintln(stderr, "neferafk auth-worker: invalid bootstrap")
		return exitFailure
	}
	if err := auth.Run(ctx, in, out, bootstrap); err != nil {
		fmt.Fprintln(stderr, "neferafk auth-worker: session ended without success")
		return exitFailure
	}
	return exitOK
}

// runVisual serves the fade/lock process spawned by the daemon: commands on fd
// 3, lifecycle events on fd 4. An active lock outlives the daemon and SIGTERM;
// it ends only after a successful unlock.
func runVisual(args []string, in, out *os.File, stderr io.Writer) int {
	if len(args) != 0 || in == nil || out == nil {
		fmt.Fprintln(stderr, "neferafk visual: internal command")
		return exitUsage
	}
	markInheritedCloexec() // the auth worker is spawned with its own pipes only
	// The visual process holds the typed secret and the PIN bootstrap: no core
	// dumps and no ptrace/proc-mem access from same-uid processes.
	if err := auth.HardenProcess(); err != nil {
		fmt.Fprintln(stderr, "neferafk visual: hardening failed")
		return exitFailure
	}
	exe, err := visual.ExecutablePath()
	if err != nil {
		fmt.Fprintf(stderr, "neferafk visual: %v\n", err)
		return exitFailure
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log := zerowrap.New(zerowrap.Config{Output: stderr})
	if err := visual.Run(ctx, visual.Config{Commands: in, Events: out, Executable: exe, Log: log}); err != nil {
		fmt.Fprintf(stderr, "neferafk visual: %v\n", err)
		return exitFailure
	}
	return exitOK
}

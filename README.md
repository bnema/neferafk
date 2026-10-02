# NeferAFK

NeferAFK is an idle daemon for Wayland compositors. It fades the screen, locks the session, turns the outputs off and suspends the machine after configurable idle delays.

It uses standard protocols only: `ext-idle-notify-v1`, `ext-session-lock-v1`, `wlr-output-power-management-v1` and systemd-logind. The lock screen and the fade use [neferclient](https://github.com/bnema/neferclient) for their Wayland surfaces and draw with [NeferGUI](https://github.com/bnema/nefergui) (Vulkan, DMA-BUF, explicit sync).

## Behaviour

- Four independent actions: fade, lock, screens off and sleep. Each has its own delay, counted from the last input. Set a delay to `off` to disable the action.
- Input undoes the fade and turns the screens back on. It never unlocks: only a successful authentication does.
- When the system is about to sleep (lid, menu, `systemctl suspend`), NeferAFK holds the suspend until the lock is confirmed.
- If the machine cannot suspend (for example, a sleep inhibitor is active), fade, lock and screens off still run and the sleep action is skipped with a warning.
- Typed secrets go from the lock screen straight to an isolated authentication process. They never pass through the daemon.

## Requirements

- Linux with systemd-logind.
- A compositor that advertises `ext_idle_notifier_v1` (v2), `ext_session_lock_manager_v1`, `zwp_linux_dmabuf_v1` (v4), `wp_linux_drm_syncobj_manager_v1`, and `zwlr_output_power_manager_v1` when screens off is enabled.
- Password mode needs a PAM service file, `/etc/pam.d/neferafk`. The Arch package installs it; otherwise create it with:

  ```
  auth include login
  ```

## Install

On Arch Linux, install from the AUR:

```sh
paru -S neferafk-bin   # latest release, pre-built
paru -S neferafk-git   # latest main, built from source
```

Pre-built archives are on the [releases page](https://github.com/bnema/neferafk/releases). To build from a checkout:

```sh
CGO_ENABLED=0 go build -o bin/neferafk ./cmd/neferafk   # builds bin/neferafk
make pkg                                               # builds an Arch package of HEAD into dist/
make install                                           # builds that package and installs it with pacman
```

The packages install `/etc/pam.d/neferafk` for password mode.

## Usage

```
neferafk run [--config FILE]   run the daemon
neferafk lock                  lock now
neferafk status                print the daemon state
neferafk validate-config FILE  check a configuration file
neferafk version               print the version
```

Start `neferafk run` from your compositor's startup. The daemon writes its log to `$XDG_STATE_HOME/neferafk/logs/daemon.log` (default `~/.local/state/neferafk/logs/daemon.log`).

## Configuration

The file is `$XDG_CONFIG_HOME/neferafk/config` (default `~/.config/neferafk/config`); `examples/config` is a commented starting point. It uses flat `key = value` lines, and `#` starts a comment. Changes are applied live. Two cases need a daemon restart: enabling screens off, and enabling lock or sleep when both were off at startup.

```
fade.after = 6m
fade.duration = 2s
lock.after = 5m
screens.off-after = 7m
sleep.after = 20m
auth.mode = password
```

| Key | Values | Default |
|---|---|---|
| `fade.after` | duration or `off` | `6m` |
| `fade.duration` | duration | `2s` |
| `lock.after` | duration or `off` | `5m` |
| `screens.off-after` | duration or `off` | `7m` |
| `sleep.after` | duration or `off` | `20m` |
| `auth.mode` | `password` or `pin` | `password` |
| `auth.pin-source` | `env` or `pass` | none |
| `auth.pin-env` | environment variable name (with `env`) | none |
| `auth.pin-entry` | `pass` entry (with `pass`) | none |

Durations use Go syntax (`30s`, `5m`, `1h`), up to 24h. The order of the actions comes from their delays: for example, `lock.after = 20s` with `screens.off-after = 50s` turns the screens off 30 seconds after the lock.

PIN mode compares a 6 to 32 digit PIN read from the daemon's environment or from `pass show ENTRY`. If the PIN source is unavailable or invalid, the lock asks for the password through PAM.

With the defaults, the lock comes first and the fade then darkens the lock screen before the screens go off. Set `fade.after` below `lock.after` to fade before locking.

## Development

```sh
make check        # build, vet, tests, handwritten-double check, staticcheck
make race         # race tests (CGO_ENABLED=1)
make mocks-check  # Mockery v3 mocks are up to date
```

The lock and fade loops also have headless tests against a nested NeferWL compositor (two virtual outputs, a GPU render node needed). They never touch the running session:

```sh
NEFERAFK_HEADLESS=/usr/sbin/neferwl go test -run Headless ./internal/adapters/visual/
```

## License

[GNU GPL v3](LICENSE).

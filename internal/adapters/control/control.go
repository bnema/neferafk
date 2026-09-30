// Package control is the tiny private local control socket. Line protocol, one
// request per connection:
//
//	-> "lock\n" | "status\n"
//	<- "ok generation=N acquiring=B protected=B faded=B off=B\n" | "err <reason>\n"
//
// Only the same UID (SO_PEERCRED) is served, the directory is 0700 and the
// socket 0600. There is deliberately no unlock command: unknown words, extra
// arguments and oversize lines are rejected.
package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/zerowrap"
	"golang.org/x/sys/unix"
)

const (
	maxLine       = 16
	ioTimeout     = 2 * time.Second
	replyTimeout  = 3 * time.Second
	maxConcurrent = 8
)

// DefaultPath is $XDG_RUNTIME_DIR/neferafk/control.sock.
func DefaultPath() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" || !filepath.IsAbs(dir) {
		return "", errors.New("XDG_RUNTIME_DIR is not set to an absolute path")
	}
	return filepath.Join(dir, "neferafk", "control.sock"), nil
}

// Server accepts control connections and forwards them to the policy owner.
type Server struct {
	ln       *net.UnixListener
	path     string
	uid      uint32
	requests chan<- ports.ControlRequest
	log      zerowrap.Logger
}

// Listen prepares the private directory (must be owned by this process's UID)
// and socket. uid is the only peer UID served; production passes os.Getuid().
func Listen(path string, uid uint32, requests chan<- ports.ControlRequest, log zerowrap.Logger) (*Server, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || st.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("control directory %s is not a directory owned by the current user", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, errors.New("another neferafk daemon is already running")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return &Server{ln: ln, path: path, uid: uid, requests: requests, log: log.WithField("component", "control")}, nil
}

// Close releases the listener and removes the socket without serving (startup
// failure path). Serve does this itself on cancellation.
func (s *Server) Close() error {
	err := s.ln.Close()
	if e := os.Remove(s.path); e != nil && !errors.Is(e, os.ErrNotExist) {
		err = errors.Join(err, e)
	}
	return err
}

// Serve accepts until ctx ends, then removes the socket. It always returns nil
// on cancellation.
func (s *Server) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { s.ln.Close() })
	defer stop()
	defer os.Remove(s.path)
	slots := make(chan struct{}, maxConcurrent)
	for {
		c, err := s.ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				s.serve(ctx, c)
			}()
		default:
			c.Close() // shed load instead of queueing
		}
	}
}

func peerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) { cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return cred.Uid, nil
}

// parse maps a request line to a kind; anything else is rejected.
func parse(line string) (ports.ControlKind, bool) {
	switch line {
	case "lock":
		return ports.ControlLock, true
	case "status":
		return ports.ControlStatus, true
	}
	return 0, false
}

func (s *Server) serve(ctx context.Context, c *net.UnixConn) {
	defer c.Close()
	uid, err := peerUID(c)
	if err != nil || uid != s.uid {
		s.log.Warn().Msg("control peer rejected")
		return // no reply for foreign peers
	}
	_ = c.SetDeadline(time.Now().Add(ioTimeout))
	line, err := bufio.NewReaderSize(io.LimitReader(c, maxLine+1), maxLine+1).ReadString('\n')
	line = strings.TrimSuffix(line, "\n")
	kind, ok := parse(line)
	if err != nil || len(line) > maxLine || !ok {
		fmt.Fprint(c, "err unknown command\n")
		return
	}
	reply := make(chan ports.ControlReply, 1)
	select {
	case s.requests <- ports.ControlRequest{Kind: kind, Reply: reply}:
	case <-ctx.Done():
		return
	case <-time.After(replyTimeout):
		fmt.Fprint(c, "err busy\n")
		return
	}
	select {
	case r := <-reply:
		_ = c.SetWriteDeadline(time.Now().Add(ioTimeout))
		fmt.Fprint(c, format(r))
	case <-ctx.Done():
	case <-time.After(replyTimeout):
		fmt.Fprint(c, "err busy\n")
	}
}

func format(r ports.ControlReply) string {
	if r.Err != "" {
		return "err " + strings.NewReplacer("\n", " ", "\r", " ").Replace(r.Err) + "\n"
	}
	return fmt.Sprintf("ok generation=%d acquiring=%t protected=%t faded=%t off=%t\n", r.Generation, r.Acquiring, r.Protected, r.Faded, r.Off)
}

// Do sends one command to the daemon and returns its reply line without the
// trailing newline. An "err ..." reply is returned as an error.
func Do(ctx context.Context, path, command string) (string, error) {
	var d net.Dialer
	ctx, cancel := context.WithTimeout(ctx, 2*replyTimeout)
	defer cancel()
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return "", fmt.Errorf("daemon not reachable: %w", err)
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(c, "%s\n", command); err != nil {
		return "", err
	}
	line, err := bufio.NewReaderSize(io.LimitReader(c, 256), 256).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("no reply from daemon: %w", err)
	}
	line = strings.TrimSuffix(line, "\n")
	if rest, ok := strings.CutPrefix(line, "err "); ok {
		return "", errors.New(rest)
	}
	return line, nil
}

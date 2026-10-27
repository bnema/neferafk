package logind

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"bnema.dev/zerobus"
	"bnema.dev/zerowrap"
)

// Wire types stay local to the adapter. Replies are decoded and descriptors
// transferred before returning across this seam.
type objectPath string

func (p objectPath) IsValid() bool {
	if p == "/" {
		return true
	}
	if !strings.HasPrefix(string(p), "/") {
		return false
	}
	for _, part := range strings.Split(string(p)[1:], "/") {
		if part == "" {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
				return false
			}
		}
	}
	return true
}

type variant struct{ value any }

func makeVariant(v any) variant { return variant{v} }
func (v variant) Value() any    { return v.value }

type signal struct {
	Sender string
	Path   objectPath
	Name   string
	Body   []any
}
type suspendResult struct{ Err error }
type signalMatch struct {
	Sender    string
	Path      objectPath
	Interface string
}

// bus is an adapter-local seam; newAdapter consumes its connection.
type bus interface {
	call(context.Context, string, objectPath, string, ...any) ([]any, error)
	suspend(context.Context, string) <-chan *suspendResult
	match(context.Context, signalMatch) error
	signals(chan<- *signal)
	removeSignals(chan<- *signal)
	unixFDs() bool
	close() error
}
type connection struct {
	d    *zerobus.Dispatcher
	out  chan<- *signal
	stop chan struct{}
	wg   sync.WaitGroup
	subs []*zerobus.Subscription
	log  zerowrap.Logger
}

func newConnection(d *zerobus.Dispatcher, log zerowrap.Logger) *connection {
	return &connection{d: d, stop: make(chan struct{}), log: log.WithField("component", "logind")}
}

func (c *connection) call(ctx context.Context, destination string, path objectPath, method string, args ...any) ([]any, error) {
	i := strings.LastIndexByte(method, '.')
	if i < 0 {
		return nil, errors.New("invalid method")
	}
	sig := ""
	for _, arg := range args {
		switch arg.(type) {
		case string:
			sig += "s"
		case uint32:
			sig += "u"
		default:
			return nil, errors.New("unsupported argument")
		}
	}
	e := c.d.NewCall(destination, string(path), method[:i], method[i+1:], sig)
	for _, arg := range args {
		switch v := arg.(type) {
		case string:
			e.Str(v)
		case uint32:
			e.Uint32(v)
		}
	}
	m, err := c.d.Call(ctx)
	if m != nil {
		defer m.Close()
	}
	if err != nil {
		return nil, err
	}
	if m.Signature == "h" && method != managerInterface+".Inhibit" {
		return nil, errors.New("unexpected descriptor reply")
	}
	return decodeReply(m)
}

func decodeReply(m *zerobus.Msg) ([]any, error) {
	r := m.Body()
	var value any
	switch m.Signature {
	case "":
		if r.Done() {
			return nil, nil
		}
	case "s":
		value = r.Str()
	case "o":
		value = objectPath(r.ObjectPath())
	case "v":
		var v any
		switch r.Variant() {
		case "s":
			v = r.Str()
		case "t":
			v = r.Uint64()
		case "(uo)":
			r.Struct()
			v = []any{r.Uint32(), objectPath(r.ObjectPath())}
		case "(so)":
			r.Struct()
			v = []any{r.Str(), objectPath(r.ObjectPath())}
		default:
			return nil, errors.New("unsupported property variant")
		}
		value = variant{v}
	case "h":
		index := r.UnixFD()
		if !r.Done() {
			return nil, errors.New("invalid inhibitor reply")
		}
		f, err := m.TakeFD(index)
		if err != nil {
			return nil, err
		}
		return []any{f}, nil
	default:
		return nil, fmt.Errorf("unexpected reply signature %q", m.Signature)
	}
	if !r.Done() {
		return nil, errors.New("invalid reply body")
	}
	return []any{value}, nil
}
func (c *connection) suspend(ctx context.Context, owner string) <-chan *suspendResult {
	out := make(chan *suspendResult, 1)
	c.d.NewCall(owner, string(managerPath), managerInterface, "Suspend", "b").Bool(false)
	p, err := c.d.Go()
	if err != nil {
		out <- &suspendResult{err}
		return out
	}
	c.wg.Go(func() {
		defer p.Cancel()
		select {
		case <-ctx.Done():
			out <- &suspendResult{ctx.Err()}
			return
		case <-c.stop:
			return
		case <-p.Done():
		}
		if err := ctx.Err(); err != nil {
			out <- &suspendResult{err}
			return
		}
		m, err := p.Result()
		if m != nil {
			m.Close()
		}
		out <- &suspendResult{err}
	})
	return out
}
func (c *connection) match(ctx context.Context, rule signalMatch) error {
	sub, err := c.d.Subscribe(ctx, zerobus.Match{Sender: rule.Sender, Path: string(rule.Path), Interface: rule.Interface}, 32)
	if err != nil {
		return err
	}
	c.subs = append(c.subs, sub)
	c.wg.Go(func() {
		for {
			select {
			case <-c.stop:
				return
			case m, ok := <-sub.C:
				if !ok {
					select {
					case c.out <- nil:
					case <-c.stop:
					}
					return
				}
				s := decodeSignal(m)
				m.Close()
				if s != nil {
					select {
					case c.out <- s:
					case <-c.stop:
						return
					}
				}
			}
		}
	})
	return nil
}
func decodeSignal(m *zerobus.Msg) *signal {
	s := &signal{Sender: m.Sender, Path: objectPath(m.Path), Name: m.Interface + "." + m.Member}
	r := m.Body()
	switch m.Signature {
	case "":
	case "b":
		s.Body = []any{r.Bool()}
	default:
		return nil
	}
	if !r.Done() {
		return nil
	}
	return s
}
func (c *connection) signals(ch chan<- *signal)    { c.out = ch }
func (c *connection) removeSignals(chan<- *signal) {}
func (c *connection) unixFDs() bool                { return c.d.UnixFDs() }
func (c *connection) close() error {
	close(c.stop)
	for _, sub := range c.subs {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		_ = sub.Close(ctx)
		cancel()
		if dropped := sub.Dropped(); dropped != 0 {
			c.log.Warn().Uint64("dropped", dropped).Msg("logind signals dropped")
		}
	}
	err := c.d.Close()
	c.wg.Wait()
	return err
}

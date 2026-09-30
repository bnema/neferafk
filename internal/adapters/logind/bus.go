package logind

import (
	"context"
	"github.com/godbus/dbus/v5"
)

// bus is an adapter-local godbus seam. newAdapter consumes its connection.
type bus interface {
	call(context.Context, string, dbus.ObjectPath, string, ...any) ([]any, error)
	suspend(context.Context, string) <-chan *dbus.Call
	match(context.Context, string) error
	signals(chan<- *dbus.Signal)
	removeSignals(chan<- *dbus.Signal)
	unixFDs() bool
	close() error
}
type connection struct{ conn *dbus.Conn }

func (c connection) call(ctx context.Context, destination string, path dbus.ObjectPath, method string, args ...any) ([]any, error) {
	call := c.conn.Object(destination, path).CallWithContext(ctx, method, 0, args...)
	return call.Body, call.Err
}
func (c connection) suspend(ctx context.Context, owner string) <-chan *dbus.Call {
	call := c.conn.Object(owner, managerPath).GoWithContext(ctx, managerInterface+".Suspend", 0, make(chan *dbus.Call, 1), false)
	return call.Done
}
func (c connection) match(ctx context.Context, rule string) error {
	return c.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.AddMatch", 0, rule).Err
}
func (c connection) signals(ch chan<- *dbus.Signal)       { c.conn.Signal(ch) }
func (c connection) removeSignals(ch chan<- *dbus.Signal) { c.conn.RemoveSignal(ch) }
func (c connection) unixFDs() bool                        { return c.conn.SupportsUnixFDs() }
func (c connection) close() error                         { return c.conn.Close() }

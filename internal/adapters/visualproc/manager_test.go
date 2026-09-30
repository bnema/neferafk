package visualproc

import (
	"context"
	"errors"
	"io"
	"testing"

	portsmocks "github.com/bnema/neferafk/internal/mocks/ports"
	"github.com/bnema/neferafk/internal/ports"
	"github.com/bnema/zerowrap"
	"github.com/stretchr/testify/mock"
)

type rig struct {
	launcher *portsmocks.MockVisualLauncher
	out      chan ports.VisualEvent
	m        *Manager
}

func newRig(t *testing.T) *rig {
	r := &rig{launcher: portsmocks.NewMockVisualLauncher(t), out: make(chan ports.VisualEvent, 8)}
	r.m = NewManager(context.Background(), r.launcher, r.out, zerowrap.New(zerowrap.Config{Output: io.Discard}))
	return r
}

func newSession(t *testing.T) (*portsmocks.MockVisualSession, chan ports.VisualEvent) {
	s := portsmocks.NewMockVisualSession(t)
	ev := make(chan ports.VisualEvent, 4)
	s.EXPECT().Events().Return((<-chan ports.VisualEvent)(ev)).Maybe()
	return s, ev
}

func TestFadeRevealNeverSpawns(t *testing.T) {
	r := newRig(t) // launcher has no expectations
	if err := r.m.Fade(context.Background(), ports.VisualFade{}); err != nil {
		t.Fatal(err)
	}
}

func TestLazySpawnReuseAndLockIdempotencePerGeneration(t *testing.T) {
	r := newRig(t)
	s, ev := newSession(t)
	spawn := ports.VisualSpawn{OmitEnv: []string{"PIN"}}
	r.launcher.EXPECT().Launch(mock.Anything, spawn).Return(s, nil).Once()
	s.EXPECT().Send(ports.VisualFade{Black: true, Spawn: spawn}).Return(nil).Once()
	lock := ports.VisualLock{Generation: 1, Auth: ports.AuthBootstrap{Generation: 1, EnvPIN: []byte("123456")}, Spawn: spawn}
	s.EXPECT().Send(mock.MatchedBy(func(c ports.VisualCommand) bool { l, ok := c.(ports.VisualLock); return ok && l.Generation == 1 })).Return(nil).Once()
	s.EXPECT().Send(mock.MatchedBy(func(c ports.VisualCommand) bool { l, ok := c.(ports.VisualLock); return ok && l.Generation == 2 })).Return(nil).Once()
	if err := r.m.Fade(context.Background(), ports.VisualFade{Black: true, Spawn: spawn}); err != nil {
		t.Fatal(err)
	}
	pin := lock.Auth.EnvPIN
	if err := r.m.Lock(context.Background(), lock); err != nil {
		t.Fatal(err)
	}
	if string(pin) == "123456" {
		t.Fatal("manager did not wipe the PIN it owns")
	}
	if err := r.m.Lock(context.Background(), ports.VisualLock{Generation: 1, Auth: ports.AuthBootstrap{Generation: 1}}); err != nil {
		t.Fatal(err) // same generation: no second send (mock enforces)
	}
	if err := r.m.Lock(context.Background(), ports.VisualLock{Generation: 2, Auth: ports.AuthBootstrap{Generation: 2}}); err != nil {
		t.Fatal(err)
	}
	// Lifecycle events are forwarded in order.
	ev <- ports.LockConfirmed{Generation: 2}
	if got := <-r.out; got != (ports.LockConfirmed{Generation: 2}) {
		t.Fatalf("%#v", got)
	}
	s.EXPECT().Close().Return(nil).Once()
	if err := r.m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCrashedVisualIsReplacedOnNextLock(t *testing.T) {
	r := newRig(t)
	s1, ev1 := newSession(t)
	s2, _ := newSession(t)
	r.launcher.EXPECT().Launch(mock.Anything, mock.Anything).Return(s1, nil).Once()
	r.launcher.EXPECT().Launch(mock.Anything, mock.Anything).Return(s2, nil).Once()
	s1.EXPECT().Send(mock.Anything).Return(nil).Once()
	s1.EXPECT().Close().Return(nil).Once()
	s2.EXPECT().Send(mock.Anything).Return(nil).Once()
	s2.EXPECT().Detach().Return(nil).Once()
	lock := ports.VisualLock{Generation: 1, Auth: ports.AuthBootstrap{Generation: 1}}
	if err := r.m.Lock(context.Background(), lock); err != nil {
		t.Fatal(err)
	}
	ev1 <- ports.VisualExited{Err: errors.New("crash")}
	if got, ok := (<-r.out).(ports.VisualExited); !ok || got.Err == nil {
		t.Fatalf("%#v", got)
	}
	// Same generation, but the process is gone: a fresh visual re-acquires.
	if err := r.m.Lock(context.Background(), lock); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Detach(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFailureRetriesOnFreshProcessThenReports(t *testing.T) {
	r := newRig(t)
	s1, _ := newSession(t)
	s2, _ := newSession(t)
	r.launcher.EXPECT().Launch(mock.Anything, mock.Anything).Return(s1, nil).Once()
	r.launcher.EXPECT().Launch(mock.Anything, mock.Anything).Return(s2, nil).Once()
	s1.EXPECT().Send(mock.Anything).Return(errors.New("broken pipe")).Once()
	s1.EXPECT().Close().Return(nil).Once()
	s2.EXPECT().Send(mock.Anything).Return(errors.New("broken pipe")).Once()
	s2.EXPECT().Close().Return(nil).Once()
	err := r.m.Lock(context.Background(), ports.VisualLock{Generation: 1, Auth: ports.AuthBootstrap{Generation: 1}})
	if err == nil {
		t.Fatal("unreachable visual reported success")
	}
	r.launcher.EXPECT().Launch(mock.Anything, mock.Anything).Return(nil, errors.New("exec failed")).Once()
	if err := r.m.Lock(context.Background(), ports.VisualLock{Generation: 1, Auth: ports.AuthBootstrap{Generation: 1}}); err == nil {
		t.Fatal("launch failure swallowed")
	}
}

func TestReleasedLockAllowsRelockOfSameGenerationNumber(t *testing.T) {
	r := newRig(t)
	s, ev := newSession(t)
	r.launcher.EXPECT().Launch(mock.Anything, mock.Anything).Return(s, nil).Once()
	s.EXPECT().Send(mock.Anything).Return(nil).Twice()
	s.EXPECT().Close().Return(nil).Once()
	lock := ports.VisualLock{Generation: 1, Auth: ports.AuthBootstrap{Generation: 1}}
	r.m.Lock(context.Background(), lock)
	ev <- ports.LockReleased{Generation: 1}
	<-r.out
	if err := r.m.Lock(context.Background(), lock); err != nil {
		t.Fatal(err)
	}
	r.m.Close()
}

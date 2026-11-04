package runner

import (
	"context"
	"fmt"
	"sync"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// FakeTarget is an in-memory TargetBackend for tests: no tunnel, no ssh. Handler answers commands
// (nil: echo with exit 0); Down makes every call fail like a vanished machine.
type FakeTarget struct {
	Handler func(cmd string) (output string, exit int)

	mu      sync.Mutex
	Cmds    []string
	Puts    [][2]string // local, dest
	Opened  int
	Closed  int
	Down    bool
	LastKey string // the key file contents the daemon wrote for the last Exec
}

type fakeTunnel struct{ f *FakeTarget }

func (t fakeTunnel) Addr() (string, int) { return "127.0.0.1", 2222 }
func (t fakeTunnel) Close() error        { t.f.mu.Lock(); t.f.Closed++; t.f.mu.Unlock(); return nil }

func (f *FakeTarget) OpenTunnel(_ context.Context, _ proto.TargetConn) (Tunnel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return nil, fmt.Errorf("fake target is down")
	}
	f.Opened++
	return fakeTunnel{f}, nil
}

func (f *FakeTarget) Exec(_ context.Context, ep Endpoint, cmd string) (ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return ExecResult{}, fmt.Errorf("%w: fake target is down", ErrTargetLost)
	}
	f.Cmds = append(f.Cmds, cmd)
	if b, err := readFile(ep.KeyFile); err == nil {
		f.LastKey = string(b)
	}
	if f.Handler == nil {
		return ExecResult{Output: cmd + "\n"}, nil
	}
	out, code := f.Handler(cmd)
	return ExecResult{Output: out, ExitCode: code}, nil
}

func (f *FakeTarget) Put(_ context.Context, _ Endpoint, local, dest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return fmt.Errorf("%w: fake target is down", ErrTargetLost)
	}
	f.Puts = append(f.Puts, [2]string{local, dest})
	return nil
}

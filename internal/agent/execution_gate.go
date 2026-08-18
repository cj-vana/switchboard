package agent

import "context"

// ExecutionGate is a context-aware one-at-a-time lease. It is intentionally
// narrower than a mutex: a canceled delegate waiting behind another task can
// leave immediately instead of leaking a worker until the holder finishes.
type ExecutionGate struct {
	lease chan struct{}
}

func NewExecutionGate() *ExecutionGate {
	return &ExecutionGate{lease: make(chan struct{}, 1)}
}

func (g *ExecutionGate) Acquire(ctx context.Context) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	select {
	case g.lease <- struct{}{}:
		return func() { <-g.lease }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

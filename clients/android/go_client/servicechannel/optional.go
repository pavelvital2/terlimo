package servicechannel

import (
	"context"
	"sync"
)

// Optional reads use the existing connection only and yield to ordinary requests.
// The Runner owns their lifetime; this gate creates no worker or retry loop.
type optionalReadKey struct{}
type optionalRead struct{ cancel context.CancelFunc }
type optionalPriority struct {
	mu         sync.Mutex
	active     *optionalRead
	foreground int
}

func (d *Doer) BeginOptional(ctx context.Context) (context.Context, func(), bool) {
	d.priority.mu.Lock()
	defer d.priority.mu.Unlock()
	if ctx.Err() != nil || d.priority.active != nil || d.priority.foreground != 0 {
		return ctx, func() {}, false
	}
	child, cancel := context.WithCancel(ctx)
	lease := &optionalRead{cancel: cancel}
	d.priority.active = lease
	return context.WithValue(child, optionalReadKey{}, lease), func() {
		cancel()
		d.priority.mu.Lock()
		defer d.priority.mu.Unlock()
		if d.priority.active == lease {
			d.priority.active = nil
		}
	}, true
}

// PreemptOptional is called at control receipt, before dispatch can block on I/O.
func (d *Doer) PreemptOptional() {
	d.priority.mu.Lock()
	defer d.priority.mu.Unlock()
	if d.priority.active != nil {
		d.priority.active.cancel()
	}
}

func (d *Doer) enterRequest(ctx context.Context) func() {
	if _, ok := ctx.Value(optionalReadKey{}).(*optionalRead); ok {
		return func() {}
	}
	d.priority.mu.Lock()
	d.priority.foreground++
	if d.priority.active != nil {
		d.priority.active.cancel()
	}
	d.priority.mu.Unlock()
	return func() { d.priority.mu.Lock(); d.priority.foreground--; d.priority.mu.Unlock() }
}

func optionalRequest(ctx context.Context) bool {
	_, ok := ctx.Value(optionalReadKey{}).(*optionalRead)
	return ok
}

package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type catalogCycleKey struct{}
type catalogPublishedKey struct{}

func catalogProgressComplete(ctx context.Context) bool {
	flag, _ := ctx.Value(catalogPublishedKey{}).(*atomic.Bool)
	return flag != nil && flag.Load()
}

func markCatalogPublished(ctx context.Context) {
	if flag, ok := ctx.Value(catalogPublishedKey{}).(*atomic.Bool); ok {
		flag.Store(true)
	}
}

func validCatalogCycle(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func catalogCycleFromContext(ctx context.Context) string {
	value, _ := ctx.Value(catalogCycleKey{}).(string)
	return value
}

// catalogOperations owns correlation and cancellation only. The existing Runner
// remains the sole worker. Pending host requests never relabel an in-flight result.
type catalogOperations struct {
	mu                               sync.Mutex
	initial, pending, latest, active string
	started                          bool
	ctx                              context.Context
	stop                             context.CancelFunc
}

func (o *catalogOperations) initialize(initial string) {
	if validCatalogCycle(initial) {
		o.initial, o.latest = initial, initial
	}
}

func (o *catalogOperations) request(cycle string) bool {
	if !validCatalogCycle(cycle) {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if cycle == o.latest || cycle == o.active {
		return false
	}
	o.latest, o.pending = cycle, cycle
	if o.active != "" && o.stop != nil {
		o.stop()
	}
	return true
}

func (o *catalogOperations) cancel(cycle string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pending == cycle {
		o.pending = ""
	}
	if !o.started && o.initial == cycle {
		o.initial = ""
	}
	if o.active == cycle && o.stop != nil {
		o.stop()
	}
}

func (o *catalogOperations) begin(parent context.Context, manual bool) (context.Context, func() bool) {
	o.mu.Lock()
	cycle := ""
	if !o.started {
		o.started = true
		cycle, o.initial = o.initial, ""
	} else if manual {
		cycle, o.pending = o.pending, ""
	}
	ctx, stop := parent, func() {}
	if cycle != "" {
		tagged := context.WithValue(parent, catalogCycleKey{}, cycle)
		tagged = context.WithValue(tagged, catalogPublishedKey{}, &atomic.Bool{})
		ctx, stop = context.WithTimeout(tagged, 65*time.Second)
	} else if manual && o.latest != "" {
		// The queued wake can outlive a cancel_catalog received before dispatch.
		// Consume it without turning a canceled explicit operation into a new poll.
		ctx, stop = context.WithCancel(parent)
		stop()
	}
	o.active, o.ctx, o.stop = cycle, ctx, stop
	o.mu.Unlock()
	return ctx, func() bool {
		stop()
		o.mu.Lock()
		defer o.mu.Unlock()
		o.active, o.ctx, o.stop = "", nil, nil
		return o.pending != ""
	}
}

func (o *catalogOperations) current() context.Context {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ctx
}

func (m *managedMobile) beginCatalogAttempt(parent context.Context, manual bool) (context.Context, func()) {
	ctx, finish := m.catalogOps.begin(parent, manual)
	return ctx, func() {
		if finish() {
			m.runner.TriggerManual()
		}
	}
}

func (m *managedMobile) catalogPublicationContext() context.Context {
	if ctx := m.catalogOps.current(); ctx != nil {
		return ctx
	}
	m.mu.Lock()
	ctx := m.runCtx
	m.mu.Unlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

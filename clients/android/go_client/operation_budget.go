package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

// operationBudget is the explicit controller for one bounded preparation operation
// (Connect / onboarding hour / switch). It solves three problems the previous context
// decorator could not:
//
//   - the context it exposes has a STABLE Deadline (none); its cancellation is driven by
//     the controller, and a paused user wait stops the controller clock without ever
//     changing the context contract;
//   - the wait is transitioned BEFORE the challenge is emitted, so a slow bridge cannot
//     let the timer expire first, and timer callbacks are fenced by a generation so a
//     stop/reset can never fire a stale deadline;
//   - preparation workers receive only a correlated handle (via context value): they can
//     pause the operation while it prepares, and the handle is invalidated at readiness,
//     so a post-readiness CAPTCHA (another runtime, an old switch, an add-stream worker)
//     never pauses a new operation. The runtime lifetime is untouched.
type operationBudgetKey struct{}

type operationBudget struct {
	mu      sync.Mutex
	parent  context.Context
	inner   context.Context
	cancel  context.CancelCauseFunc
	ctx     context.Context
	linked  bool
	expired bool
	paused  int
	base    *budgetTimer
	tracked map[*budgetTimer]struct{}
}

// budgetTimer owns its own callback identity: arming or stopping one timer can never
// invalidate the callback of the base timer or of a sibling tracked timeout. Controller-
// wide lifecycle (Invalidate/stop) is expressed by the shared `linked` flag.
type budgetTimer struct {
	remaining  time.Duration
	deadline   time.Time
	timer      *time.Timer
	fired      bool
	generation uint64
	cancel     context.CancelCauseFunc
}

// newOperationBudget derives the operation context. The returned stop cancels the
// operation (context.Canceled), like the WithTimeout cancel it replaces.
func newOperationBudget(parent context.Context, d time.Duration) (context.Context, *operationBudget, context.CancelFunc) {
	if d < 0 {
		d = 0
	}
	inner, cancel := context.WithCancelCause(parent)
	b := &operationBudget{parent: parent, inner: inner, cancel: cancel, linked: true, tracked: map[*budgetTimer]struct{}{}}
	b.ctx = &operationBudgetContext{inner: inner, budget: b}
	b.base = &budgetTimer{remaining: d, deadline: time.Now().Add(d)}
	b.mu.Lock()
	b.armLocked(b.base)
	b.mu.Unlock()
	stop := func() {
		b.mu.Lock()
		b.unlinkLocked()
		b.mu.Unlock()
		cancel(context.Canceled)
	}
	return b.ctx, b, stop
}

func (b *operationBudget) armLocked(t *budgetTimer) {
	t.generation++
	gen := t.generation
	t.fired = false
	t.timer = time.AfterFunc(t.remaining, func() { b.fire(t, gen) })
}

func (b *operationBudget) fire(t *budgetTimer, gen uint64) {
	b.mu.Lock()
	if gen != t.generation || b.paused > 0 || b.expired || !b.linked || t.fired {
		b.mu.Unlock()
		return
	}
	t.fired = true
	if t == b.base {
		b.expired = true
		b.mu.Unlock()
		b.cancel(context.DeadlineExceeded)
		return
	}
	cancel := t.cancel
	b.mu.Unlock()
	if cancel != nil {
		cancel(context.DeadlineExceeded)
	}
}

// PauseForWait transitions the operation into the bounded user wait:
//
//	paused, true  — link is active: the clock is paused (caller must ResumeAfterWait);
//	false, true   — link is invalidated (post-readiness): donor behavior, no pause;
//	false, false  — the budget already expired or the operation is cancelled: do not emit.
func (b *operationBudget) PauseForWait() (bool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.linked {
		return false, true
	}
	if b.expired || b.inner.Err() != nil {
		return false, false
	}
	if b.paused == 0 {
		now := time.Now()
		b.base.remaining = clampRemaining(b.base.deadline, now)
		if b.base.timer != nil {
			b.base.timer.Stop()
		}
		for t := range b.tracked {
			if t.fired {
				continue
			}
			t.remaining = clampRemaining(t.deadline, now)
			if t.timer != nil {
				t.timer.Stop()
			}
		}
	}
	b.paused++
	return true, true
}

// ResumeAfterWait restarts every paused clock with exactly the remaining time. Timers
// whose remaining time is already zero fire once, after the lock is released.
func (b *operationBudget) ResumeAfterWait() {
	b.mu.Lock()
	if b.paused == 0 {
		b.mu.Unlock()
		return
	}
	b.paused--
	if b.paused > 0 {
		b.mu.Unlock()
		return
	}
	if !b.linked || b.expired {
		b.mu.Unlock()
		return
	}
	now := time.Now()
	if b.base.remaining <= 0 {
		b.expired = true
		b.mu.Unlock()
		b.cancel(context.DeadlineExceeded)
		return
	}
	b.base.deadline = now.Add(b.base.remaining)
	b.armLocked(b.base)
	var fire []context.CancelCauseFunc
	for t := range b.tracked {
		if t.fired {
			continue
		}
		if t.remaining <= 0 {
			t.fired = true
			fire = append(fire, t.cancel)
			continue
		}
		t.deadline = now.Add(t.remaining)
		b.armLocked(t)
	}
	b.mu.Unlock()
	for _, cancel := range fire {
		if cancel != nil {
			cancel(context.DeadlineExceeded)
		}
	}
}

// Invalidate decouples the operation handle at readiness. The operation context is NOT
// cancelled: the live runtime keeps running, but later CAPTCHA prompts find no active
// link and use the donor behavior.
func (b *operationBudget) Invalidate() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unlinkLocked()
}

func (b *operationBudget) unlinkLocked() {
	if !b.linked {
		return
	}
	b.linked = false
	if b.base.timer != nil {
		b.base.timer.Stop()
	}
	for t := range b.tracked {
		if t.timer != nil {
			t.timer.Stop()
		}
	}
}

// TrackTimeout derives one bounded sub-timeout that shares the operation clock: it keeps
// its configured duration, and a paused user wait stops it too. It implements the
// onboarding.WaitPauser seam. Returns ok=false when the operation is no longer linked.
func (b *operationBudget) TrackTimeout(d time.Duration) (context.Context, context.CancelFunc, bool) {
	if d < 0 {
		d = 0
	}
	b.mu.Lock()
	if !b.linked || b.expired {
		b.mu.Unlock()
		return nil, nil, false
	}
	child, cancel := context.WithCancelCause(b.ctx)
	t := &budgetTimer{remaining: d, deadline: time.Now().Add(d), cancel: cancel}
	b.tracked[t] = struct{}{}
	if b.paused == 0 {
		b.armLocked(t)
	}
	b.mu.Unlock()
	release := func() {
		b.mu.Lock()
		t.generation++ // fence a callback that is already waiting on the lock
		delete(b.tracked, t)
		if t.timer != nil {
			t.timer.Stop()
		}
		b.mu.Unlock()
		cancel(context.Canceled)
	}
	return child, release, true
}

// WaitContext returns the bounded wait for the challenge. Its lifetime belongs to the
// live operation chain, never to a preparation context that may already be completed:
// after readiness the runtime keeps running while the preparation is invalidated and
// canceled, and its CAPTCHAs must still be solvable.
//
// Cancellation sources: the original operation parent (Disconnect/runtime stop) and a
// real (non-deadline) cancellation of the caller. The caller's own narrower deadline is
// ignored — those fixed network sub-timeouts keep their duration and only stop ticking
// while the user solves the challenge. Release and the donor timeout are local.
func (b *operationBudget) WaitContext(caller context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	waitCtx, cancelWait := context.WithCancelCause(context.Background())
	timer := time.AfterFunc(timeout, func() { cancelWait(context.DeadlineExceeded) })
	watch := func(ctx context.Context) func() bool {
		if ctx == nil {
			return func() bool { return false }
		}
		return context.AfterFunc(ctx, func() {
			cause := context.Cause(ctx)
			if errors.Is(cause, context.DeadlineExceeded) {
				return // narrower network deadline: the bounded donor wait continues
			}
			cancelWait(cause)
		})
	}
	stopParent := watch(b.parent)
	var stopCaller func() bool = func() bool { return false }
	if caller != nil && caller != b.parent {
		stopCaller = watch(caller)
	}
	release := func() {
		stopParent()
		stopCaller()
		timer.Stop()
		cancelWait(context.Canceled)
	}
	return waitCtx, release
}

func (b *operationBudget) expiredNow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.expired
}

func (b *operationBudget) pausedNow() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.paused
}

func clampRemaining(deadline, now time.Time) time.Duration {
	if remaining := deadline.Sub(now); remaining > 0 {
		return remaining
	}
	return 0
}

func budgetFromContext(ctx context.Context) *operationBudget {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(operationBudgetKey{}).(*operationBudget)
	return b
}

// linkOperationBudget hands the correlated handle of one preparation operation to the
// workers started by that operation. It only carries the pause capability; it never
// shortens the worker lifetime.
func linkOperationBudget(parent, preparation context.Context) context.Context {
	if parent == nil || preparation == nil {
		return parent
	}
	b := budgetFromContext(preparation)
	if b == nil {
		return parent
	}
	return context.WithValue(parent, operationBudgetKey{}, b)
}

func invalidatePreparationBudget(preparation context.Context) {
	if b := budgetFromContext(preparation); b != nil {
		b.Invalidate()
	}
}

type operationBudgetContext struct {
	inner  context.Context
	budget *operationBudget
}

// Deadline is always absent: the context contract requires repeated calls to return the
// same value, so a movable deadline is impossible by construction. Expiry is reported
// through Done/Err by the controller.
func (c *operationBudgetContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c *operationBudgetContext) Done() <-chan struct{} { return c.inner.Done() }

func (c *operationBudgetContext) Err() error {
	if c.budget.expiredNow() {
		return context.DeadlineExceeded
	}
	return c.inner.Err()
}

func (c *operationBudgetContext) Value(key any) any {
	if _, ok := key.(operationBudgetKey); ok {
		return c.budget
	}
	return c.inner.Value(key)
}

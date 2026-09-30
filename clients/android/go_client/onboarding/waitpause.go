package onboarding

import (
	"context"
	"time"
)

// WaitPauser is the operation-level budget handle. It lets this package keep its own
// bounded attempt timeout while a user CAPTCHA wait pauses the clock: the timeout keeps
// its configured duration and is never extended — only the time spent solving the
// challenge does not count against it. Without a pauser the ordinary context timeout is
// used unchanged.
type WaitPauser interface {
	TrackTimeout(time.Duration) (context.Context, context.CancelFunc, bool)
}

type waitPauserKey struct{}

// WithWaitPauser attaches the operation budget handle to one onboarding call chain.
func WithWaitPauser(ctx context.Context, pauser WaitPauser) context.Context {
	if pauser == nil {
		return ctx
	}
	return context.WithValue(ctx, waitPauserKey{}, pauser)
}

func waitPauser(ctx context.Context) (WaitPauser, bool) {
	pauser, ok := ctx.Value(waitPauserKey{}).(WaitPauser)
	return pauser, ok && pauser != nil
}

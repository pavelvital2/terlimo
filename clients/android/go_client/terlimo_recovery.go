package main

import (
	"context"
	"errors"
	"time"
	"wg-turn-client/onboarding"
	"wg-turn-client/servicechannel"
)

// Candidate transport is memory-only. The original Store owns last-good durability.
type mobileRecoveryTransport struct {
	*servicechannel.Doer
	durable   *servicechannel.Store
	candidate servicechannel.Seed
	noop      bool
}

func recoveryResultCode(err error) string {
	if err == nil {
		return "RECOVERY_SUCCESS"
	}
	for _, known := range []error{servicechannel.ErrRecoveryUnavailable, servicechannel.ErrRecoveryInvalid,
		servicechannel.ErrRecoverySignature, servicechannel.ErrRecoveryEnvironment,
		servicechannel.ErrRecoveryStale, servicechannel.ErrRecoveryPersist, servicechannel.ErrRecoveryCancelled} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	if errors.Is(err, context.Canceled) {
		return "RECOVERY_CANCELLED"
	}
	return "RECOVERY_NETWORK"
}

// One explicit service-only attempt, using existing auth/Doer/Runner and preparation
// budget. No catalogue, recurring loop, data VPN, enrollment, or payment operation.
func (c *managedController) runRecovery(ctx context.Context) error {
	m, err := newManagedMobile(c.start, c.public, c.sign, c.bridge, c)
	if err == nil {
		c.mobile = m
		if m.transportCloser != nil {
			defer m.transportCloser.Close()
		}
		if m.recovery == nil {
			err = servicechannel.ErrRecoveryUnavailable
		} else if !m.recovery.noop {
			err = applyRecovery(ctx, m.recovery, m.runner.AuthenticateServiceOnce)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if sendErr := c.bridge.sendContext(ctx, bridgeMessage{"type": "recovery_result", "code": recoveryResultCode(err)}); sendErr != nil {
		return sendErr
	}
	// Wait for ordinary host Stop: immediate stopped/EOF could discard the queued result.
	<-ctx.Done()
	return ctx.Err()
}

// Recovery reuses the accepted cold stages. The final status phase includes the
// durable ACK: no fresh budget after /me, and no catalog/recurring work is started.
func applyRecovery(ctx context.Context, recovery *mobileRecoveryTransport, authenticate func(context.Context) error) error {
	if recovery.Channel == nil || recovery.Channel.Catalog == nil {
		return servicechannel.ErrRecoveryUnavailable
	}
	policy := *recovery.Channel.Catalog
	operation, budget, stop := newOperationBudget(ctx, policy.ConnectTimeout+policy.DeviceTimeout+policy.StatusTimeout)
	defer stop()
	operation = recoveryDeadlineContext{onboarding.WithWaitPauser(operation, budget)}
	phases := recoveryPhases{budget: budget}
	defer phases.close()
	policy.TimeoutContext = phases.timeoutContext
	previous := recovery.Channel.Catalog
	recovery.Channel.Catalog = &policy
	defer func() { recovery.Channel.Catalog = previous }()
	if err := authenticate(operation); err != nil {
		return err
	}
	// Successful strict auth+/me must have reached status. Keep its remaining
	// active time for persistence, with the original host cancellation fence.
	if phases.stage != "subscription_status" {
		return servicechannel.ErrRecoveryUnavailable
	}
	return recovery.durable.CommitRecovery(recoveryDeadlineContext{phases.ctx}, recovery.candidate)
}

// These timers belong to this single Apply only. AUTH retains one clock across
// challenge, proof/signing, session and any scope fallback. Timers are tracked by
// the existing operation budget, so the existing CAPTCHA pauser stops them too.
type recoveryPhases struct {
	budget     *operationBudget
	stage      string
	ctx        context.Context
	stop       context.CancelFunc
	stopExpiry func() bool
}

func (p *recoveryPhases) close() {
	if p.stopExpiry != nil {
		p.stopExpiry()
	}
	if p.stop != nil {
		p.stop()
	}
}

func (p *recoveryPhases) timeoutContext(ctx context.Context, stage string, limit time.Duration) (context.Context, context.CancelFunc) {
	// A cold reconnect inside AUTH must not replenish AUTH or return to the
	// original connecting phase. It is capped by both its own 20s and AUTH's
	// remaining time. Similarly, status cannot replenish AUTH on a later call.
	rank := map[string]int{"connecting_server": 1, "checking_device": 2, "subscription_status": 3}
	if rank[stage] > rank[p.stage] && (p.ctx == nil || p.ctx.Err() == nil) {
		p.close()
		p.ctx, p.stop, _ = p.budget.TrackTimeout(limit)
		p.stage = stage
		if p.ctx != nil {
			phase := p.ctx
			p.stopExpiry = context.AfterFunc(phase, func() {
				// Also stop signing/decoding between HTTP calls when AUTH's
				// aggregate clock expires; cancellation ends the entire Apply.
				if errors.Is(context.Cause(phase), context.DeadlineExceeded) {
					p.budget.cancel(context.DeadlineExceeded)
				}
			})
		}
	}
	if p.ctx == nil {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return cancelled, func() {}
	}
	// Preserve request values (class, network, CAPTCHA handle) and honor both
	// caller cancellation and the shared phase timer. Per-call release cannot
	// cancel the aggregate timer needed by the next AUTH request.
	child, cancel := context.WithCancelCause(ctx)
	link := func(parent context.Context) func() bool {
		if parent.Err() != nil {
			cancel(context.Cause(parent))
		}
		return context.AfterFunc(parent, func() { cancel(context.Cause(parent)) })
	}
	unlink := link(p.ctx)
	var releaseLocal context.CancelFunc = func() {}
	var unlinkLocal = func() bool { return false }
	if rank[stage] < rank[p.stage] {
		if local, release, ok := p.budget.TrackTimeout(limit); ok {
			releaseLocal, unlinkLocal = release, link(local)
		} else {
			cancel(context.Canceled)
		}
	}
	return recoveryDeadlineContext{child}, func() { unlink(); unlinkLocal(); releaseLocal(); cancel(context.Canceled) }
}

// TrackTimeout uses cancellation causes to preserve the pausable clock. Expose
// timeout as DeadlineExceeded to the existing channel/result classification.
type recoveryDeadlineContext struct{ context.Context }

func (c recoveryDeadlineContext) Err() error {
	if errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

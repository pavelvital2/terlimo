package main

import (
	"context"
	"errors"
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
			operation, budget, stop := newOperationBudget(ctx, mobileHTTPTimeout)
			operation = onboarding.WithWaitPauser(operation, budget)
			err = m.runner.AuthenticateServiceOnce(operation)
			if err == nil {
				err = m.recovery.durable.CommitRecovery(operation, m.recovery.candidate)
			}
			stop()
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

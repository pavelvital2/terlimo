package servicechannel

import (
	"context"
	"time"
)

// CatalogPolicy separates establishment from the catalog request that caused it.
// Android owns the aggregate stage budgets (AUTH may contain multiple requests)
// and the whole refresh deadline. These native limits bound each blocking call;
// caller cancellation/deadlines always win. Configure before using the Channel.
type CatalogPolicy struct {
	ConnectTimeout time.Duration
	DeviceTimeout  time.Duration
	StatusTimeout  time.Duration
	CatalogTimeout time.Duration
	// Emit sends a correlated product event, not a diagnostic stderr line.
	// Failure prevents I/O for a stage the host did not receive.
	Emit func(context.Context, string) error
}

func (p *CatalogPolicy) request(class string) (string, time.Duration) {
	if p == nil || p.ConnectTimeout <= 0 {
		return "", 0
	}
	var stage string
	var budget time.Duration
	switch class {
	case "AUTH":
		stage, budget = "checking_device", p.DeviceTimeout
	case "ME":
		stage, budget = "subscription_status", p.StatusTimeout
	case "GATEWAYS":
		stage, budget = "loading_catalog", p.CatalogTimeout
	}
	if budget <= 0 {
		return "", 0
	}
	return stage, budget
}

func (p *CatalogPolicy) emit(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.Emit != nil {
		return p.Emit(ctx, stage)
	}
	return nil
}

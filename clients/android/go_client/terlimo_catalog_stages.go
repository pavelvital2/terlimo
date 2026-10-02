package main

import (
	"context"
	"time"

	"wg-turn-client/accountaccess"
	"wg-turn-client/servicechannel"
)

// These initial finite budgets accompany Android's 20/25/10/10 stage and 65s
// overall refresh limits. They leave room over observed cold dial/auth times;
// they are a candidate for device acceptance, not a latency guarantee.
// Non-catalog operations and direct HTTPS keep their existing limits.
func configureCatalogStages(transport accountaccess.Doer, bridge *managedBridge) {
	doer, ok := transport.(*servicechannel.Doer)
	if !ok || doer.Channel == nil || bridge == nil {
		return
	}
	doer.Channel.Catalog = &servicechannel.CatalogPolicy{
		ConnectTimeout: 20 * time.Second,
		DeviceTimeout:  25 * time.Second,
		StatusTimeout:  10 * time.Second,
		CatalogTimeout: 10 * time.Second,
		Emit: func(ctx context.Context, stage string) error {
			return bridge.sendContext(ctx, bridgeMessage{"type": "catalog_stage", "stage": stage})
		},
	}
}

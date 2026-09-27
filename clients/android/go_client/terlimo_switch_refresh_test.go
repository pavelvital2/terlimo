package main

import (
	"context"
	"errors"
	"testing"
	"time"
	"wg-turn-client/wlbs"
)

func TestActiveSwitchRefreshesExpiredTargetAToB(t *testing.T) { checkExpiredSwitchTarget(t, 0, 1) }
func TestActiveSwitchRefreshesExpiredTargetBToA(t *testing.T) { checkExpiredSwitchTarget(t, 1, 0) }

func TestActiveSwitchRefreshDeadlinePreservesLastGood(t *testing.T) {
	for _, active := range []int{0, 1} {
		now := time.Now().UTC().Truncate(time.Second)
		catalog := runnerTwoNodes(now)
		target := 1 - active
		catalog.Nodes[target].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
		c, _, _ := recoveryController(t, catalog)
		c.saved.SelectedNodeID = catalog.Nodes[active].NodeID
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		op := managedSwitch{ID: "11111111-1111-1111-1111-111111111111", NodeID: catalog.Nodes[target].NodeID, Revision: catalog.Revision}
		calls := 0
		_, err := c.prepareSwitchTarget(ctx, op, catalog.Nodes[active].NodeID, "reg", func(ctx context.Context, _ string) error {
			calls++
			<-ctx.Done()
			return ctx.Err()
		})
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
			t.Fatalf("deadline not propagated: calls=%d err=%v", calls, err)
		}
		if c.saved.SelectedNodeID != catalog.Nodes[active].NodeID || !c.rollbackAllowed(catalog.Nodes[active], "reg") {
			t.Fatal("deadline lost eligible last-good node")
		}
	}
}

func TestActiveSwitchRefreshFailurePreservesSelection(t *testing.T) {
	for _, active := range []int{0, 1} {
		for _, cancelled := range []bool{false, true} {
			now := time.Now().UTC().Truncate(time.Second)
			catalog := runnerTwoNodes(now)
			target := 1 - active
			catalog.Nodes[target].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
			c, _, _ := recoveryController(t, catalog)
			c.saved.SelectedNodeID = catalog.Nodes[active].NodeID
			ctx, cancel := context.WithCancel(context.Background())
			op := managedSwitch{ID: "11111111-1111-1111-1111-111111111111", NodeID: catalog.Nodes[target].NodeID, Revision: catalog.Revision}
			calls := 0
			_, err := c.prepareSwitchTarget(ctx, op, catalog.Nodes[active].NodeID, "reg", func(context.Context, string) error {
				calls++
				if cancelled {
					cancel()
					return nil
				}
				return errors.New("LEASE_CONFLICT")
			})
			cancel()
			if err == nil || calls != 1 || c.saved.SelectedNodeID != catalog.Nodes[active].NodeID {
				t.Fatal("failed refresh changed last-good selection or admitted target")
			}
			if !c.rollbackAllowed(catalog.Nodes[active], "reg") {
				t.Fatal("eligible last-good lost after failed target preparation")
			}
		}
	}
}

func checkExpiredSwitchTarget(t *testing.T, active, target int) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerTwoNodes(now)
	old.Nodes[target].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	c, client, _ := recoveryController(t, old)
	c.saved.SelectedNodeID = old.Nodes[active].NodeID
	operation := managedSwitch{ID: "11111111-1111-1111-1111-111111111111", NodeID: old.Nodes[target].NodeID, Revision: old.Revision}
	calls := 0
	client.refresh = func(payload wlbs.RefreshPayload) ([]byte, error) {
		calls++
		if payload.NodeID != old.Nodes[target].NodeID || c.saved.SelectedNodeID != old.Nodes[active].NodeID {
			t.Fatal("refresh changed selection or chose another target")
		}
		fresh := runnerTwoNodes(now)
		fresh.Revision = "8"
		fresh.Nodes[target].Access.LeaseSeq = "2"
		return recoveryRaw(fresh, "ok"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.prepareSwitchTarget(ctx, operation, old.Nodes[active].NodeID, "reg", func(ctx context.Context, id string) error {
		return c.synchronizeClientTargetRound(ctx, client, true, 3, false, id)
	})
	if err != nil || calls != 1 {
		t.Fatalf("explicit expired target was not refreshed: calls=%d code=%s", calls, managedCode(err))
	}
	if c.saved.SelectedNodeID != old.Nodes[active].NodeID {
		t.Fatal("selection committed before cutover")
	}
	if c.saved.Pending != nil || c.saved.Catalog.Revision != "8" {
		t.Fatal("refresh was not durably reconciled")
	}
}

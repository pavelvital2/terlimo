package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"wg-turn-client/wlbs"
)

func syncedTwoNodeCatalog(now time.Time) *wlbs.Catalog {
	cat := runnerTwoNodes(now)
	cat.Revision = "8"
	return cat
}

func TestSyncAccessExistingInstallationThenCurrentCatalog(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerCatalog(now)
	c, f, _ := recoveryController(t, old)
	next := syncedTwoNodeCatalog(now)
	f.sync = func(registration string) ([]byte, error) {
		if registration != "reg" || c.saved.Installation != "unchanged-key-fingerprint" || c.saved.Pending == nil || c.saved.Pending.Op != "sync_access" {
			t.Fatal("identity/pending changed before sync")
		}
		return recoveryRaw(next, "ok"), nil
	}
	f.catalog = func() ([]byte, error) {
		if c.saved.Completed == nil || c.saved.Pending == nil || c.saved.Completed.RequestID != c.saved.Pending.RequestID {
			t.Fatal("sync receipt not durable before catalog")
		}
		return recoveryRaw(next, "ok"), nil
	}
	if err := c.synchronizeClientRound(context.Background(), f, false, 3, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.trace, []string{"sync_access", "catalog"}) || c.saved.Pending != nil || c.saved.Installation != "unchanged-key-fingerprint" || len(c.saved.Catalog.Nodes) != 2 || c.saved.Catalog.SlotsUsed != 1 {
		t.Fatal("sync orchestration changed identity/seat", f.trace)
	}
}

func TestSyncAccessKeepsEmptySelectionUntilExplicitChoice(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerCatalog(now)
	c, f, _ := recoveryController(t, old)
	next := syncedTwoNodeCatalog(now)
	next.Nodes[0].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	f.sync = func(string) ([]byte, error) { return recoveryRaw(next, "ok"), nil }
	f.catalog = func() ([]byte, error) { return recoveryRaw(next, "ok"), nil }
	if err := c.synchronizeClientRound(context.Background(), f, false, 3, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.trace, []string{"sync_access", "catalog"}) || c.saved.SelectedNodeID != "" || len(c.saved.Catalog.Nodes) != 2 {
		t.Fatal("empty selection changed without explicit choice", f.trace, c.saved.SelectedNodeID)
	}
}

func TestSyncAccessExpiredSelectedAndNeighborRefreshesSelectedOnly(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerTwoNodes(now)
	c, f, _ := recoverySelectedController(t, old)
	synced := syncedTwoNodeCatalog(now)
	for i := range synced.Nodes {
		synced.Nodes[i].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	}
	f.sync = func(string) ([]byte, error) { return recoveryRaw(synced, "ok"), nil }
	f.catalog = func() ([]byte, error) {
		if c.saved.Completed == nil || c.saved.Pending == nil || c.saved.Completed.RequestID != c.saved.Pending.RequestID {
			t.Fatal("sync completion not durable before catalog")
		}
		return recoveryRaw(synced, "ok"), nil
	}
	f.refresh = func(p wlbs.RefreshPayload) ([]byte, error) {
		if c.saved.Catalog == nil || c.saved.Catalog.Revision != synced.Revision {
			t.Fatal("recovery metadata not durable before refresh")
		}
		if p.NodeID != old.Nodes[0].NodeID || p.NodeID == old.Nodes[1].NodeID {
			t.Fatalf("refresh selected wrong node: %q", p.NodeID)
		}
		fresh := syncedTwoNodeCatalog(now)
		fresh.Revision = "9"
		fresh.Nodes[0].Access.LeaseSeq = "2"
		fresh.Nodes[1].Access.ExpiresAt = synced.Nodes[1].Access.ExpiresAt
		return recoveryRaw(fresh, "ok"), nil
	}
	if err := c.synchronizeClientRound(context.Background(), f, false, 3, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.trace, []string{"sync_access", "catalog", "refresh"}) ||
		c.saved.Pending != nil || c.saved.SelectedNodeID != old.Nodes[0].NodeID ||
		c.store.Snapshot().ValidateNode("sub", "reg", old.Nodes[0].NodeID, now) != nil ||
		c.store.Snapshot().ValidateNode("sub", "reg", old.Nodes[1].NodeID, now) == nil {
		t.Fatal("bounded selected-only recovery failed", f.trace)
	}
}

func TestSyncAccessExpiredRecoveryStopsAtRoundBudget(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerTwoNodes(now)
	c, f, _ := recoverySelectedController(t, old)
	next := syncedTwoNodeCatalog(now)
	for i := range next.Nodes {
		next.Nodes[i].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	}
	f.sync = func(string) ([]byte, error) { return recoveryRaw(next, "ok"), nil }
	f.catalog = func() ([]byte, error) { return recoveryRaw(next, "ok"), nil }
	if err := c.synchronizeClientRound(context.Background(), f, false, 1, true); managedCode(err) != "RETRY_EXHAUSTED" {
		t.Fatal("round budget not enforced", err)
	}
	if !reflect.DeepEqual(f.trace, []string{"sync_access", "catalog"}) || c.saved.Catalog.Revision != "8" {
		t.Fatal("recovery lost metadata or exceeded budget", f.trace)
	}
}

func TestSyncAccessLostCatalogKeepsExactPendingAndResumesStatusFirst(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c, f, _ := recoveryController(t, runnerCatalog(now))
	next := syncedTwoNodeCatalog(now)
	f.sync = func(string) ([]byte, error) { return recoveryRaw(next, "ok"), nil }
	f.catalog = func() ([]byte, error) { return nil, io.EOF }
	if err := c.synchronizeClientRound(context.Background(), f, false, 3, true); !errors.Is(err, io.EOF) || c.saved.Pending == nil || c.saved.Completed == nil {
		t.Fatal("lost catalog erased resolved sync", err)
	}
	pending := *c.saved.Pending
	f.status = func(got wlbs.PendingOperation) ([]byte, error) {
		if got != pending {
			t.Fatal("pending ID/bytes changed")
		}
		return recoveryRaw(next, "complete"), nil
	}
	f.catalog = func() ([]byte, error) { return recoveryRaw(next, "ok"), nil }
	if err := c.synchronizeClientRound(context.Background(), f, false, 3, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.trace, []string{"sync_access", "catalog", "status", "catalog"}) || c.saved.Pending != nil || len(c.saved.Catalog.Nodes) != 2 {
		t.Fatal("resume did not use STATUS then CATALOG", f.trace)
	}
}

func TestSyncAccessResolvesOlderPendingBeforeNewMutation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerCatalog(now)
	c, f, _ := recoveryController(t, old)
	prior := wlbs.PendingOperation{RequestID: "prior-refresh", Op: "refresh_access", PayloadB64: "exact-prior-bytes"}
	c.saved.Pending = &prior
	next := syncedTwoNodeCatalog(now)
	f.status = func(got wlbs.PendingOperation) ([]byte, error) {
		if got != prior {
			t.Fatal("older pending changed")
		}
		return recoveryRaw(old, "complete"), nil
	}
	f.sync = func(string) ([]byte, error) {
		if c.saved.Pending == nil || c.saved.Pending.Op != "sync_access" || c.saved.Pending.RequestID == prior.RequestID {
			t.Fatal("new sync was not durably separated from resolved pending")
		}
		return recoveryRaw(next, "ok"), nil
	}
	f.catalog = func() ([]byte, error) { return recoveryRaw(next, "ok"), nil }
	if err := c.synchronizeClientRound(context.Background(), f, false, 3, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.trace, []string{"status", "sync_access", "catalog"}) || c.saved.Pending != nil || c.saved.Installation != "unchanged-key-fingerprint" || c.saved.Catalog.SlotsUsed != 1 {
		t.Fatal("STATUS/sync/catalog ordering or identity changed", f.trace)
	}
}

func TestSyncAccessUnknownResumePreservesOneShotIntent(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerCatalog(now)
	c, f, _ := recoveryController(t, old)
	prior := wlbs.PendingOperation{RequestID: "prior-refresh", Op: "refresh_access", PayloadB64: "exact-prior-bytes"}
	c.saved.Pending = &prior
	next := syncedTwoNodeCatalog(now)
	f.status = func(got wlbs.PendingOperation) ([]byte, error) {
		if got != prior {
			t.Fatal("older pending changed")
		}
		return []byte(`{"v":1,"status":"unknown"}`), nil
	}
	f.resume = func(got wlbs.PendingOperation) ([]byte, error) {
		if got != prior {
			t.Fatal("resume changed old ID/bytes")
		}
		return recoveryRaw(old, "ok"), nil
	}
	f.sync = func(string) ([]byte, error) { return recoveryRaw(next, "ok"), nil }
	f.catalog = func() ([]byte, error) { return recoveryRaw(next, "ok"), nil }
	if err := c.synchronizeClientRound(context.Background(), f, false, 3, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.trace, []string{"status", "resume", "sync_access", "catalog"}) || c.saved.Pending != nil || len(c.saved.Catalog.Nodes) != 2 {
		t.Fatal("one-shot sync intent lost after unknown/resume", f.trace)
	}
}

func TestSyncAccessTerminalOldPendingPreservesOneShotIntent(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerCatalog(now)
	c, f, _ := recoveryController(t, old)
	c.saved.Pending = &wlbs.PendingOperation{RequestID: "old-terminal", Op: "refresh_access", PayloadB64: "exact-old-bytes"}
	next := syncedTwoNodeCatalog(now)
	f.status = func(wlbs.PendingOperation) ([]byte, error) {
		return []byte(`{"v":1,"status":"failed","code":"GRANT_MISSING"}`), nil
	}
	f.sync = func(string) ([]byte, error) { return recoveryRaw(next, "ok"), nil }
	f.catalog = func() ([]byte, error) { return recoveryRaw(next, "ok"), nil }
	if err := c.synchronizeClientRound(context.Background(), f, false, 3, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.trace, []string{"status", "sync_access", "catalog"}) || c.saved.Pending != nil || c.saved.Installation != "unchanged-key-fingerprint" {
		t.Fatal("terminal reconciliation dropped sync intent", f.trace)
	}
}

func TestSyncAccessNeverFollowsFreshRegisterTerminal(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c, f, _ := recoveryController(t, runnerCatalog(now))
	c.saved.Pending = &wlbs.PendingOperation{RequestID: "fresh-register", Op: "register", PayloadB64: "exact-register-bytes"}
	f.status = func(wlbs.PendingOperation) ([]byte, error) {
		return []byte(`{"v":1,"status":"failed","code":"DEVICE_LIMIT_REACHED"}`), nil
	}
	if err := c.synchronizeClientRound(context.Background(), f, false, 3, true); managedCode(err) != "DEVICE_LIMIT_REACHED" {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.trace, []string{"status"}) || c.saved.Pending != nil {
		t.Fatal("fresh REGISTER terminal triggered sync")
	}
}

func TestSyncAccessNeverBypassesEligibilityTerminal(t *testing.T) {
	for _, code := range []string{"DEVICE_REVOKED", "SUBSCRIPTION_EXPIRED"} {
		t.Run(code, func(t *testing.T) {
			c, f, _ := recoveryController(t, runnerCatalog(time.Now().UTC().Truncate(time.Second)))
			c.saved.Pending = &wlbs.PendingOperation{RequestID: "old-operation", Op: "refresh_access", PayloadB64: "exact-old-bytes"}
			f.status = func(wlbs.PendingOperation) ([]byte, error) {
				return []byte(`{"v":1,"status":"failed","code":"` + code + `"}`), nil
			}
			if err := c.synchronizeClientRound(context.Background(), f, false, 3, true); managedCode(err) != code {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f.trace, []string{"status"}) || c.saved.Pending != nil {
				t.Fatal("eligibility terminal triggered sync")
			}
		})
	}
}

type freshRegisterClient struct {
	*recoveryClient
	raw []byte
}

func (f *freshRegisterClient) Register(ctx context.Context) ([]byte, *wlbs.PendingOperation, error) {
	f.trace = append(f.trace, "register")
	id, _ := wlbs.NewID()
	payload, _ := json.Marshal(map[string]string{"op": "register"})
	p := &wlbs.PendingOperation{RequestID: id.String(), Op: "register", PayloadB64: wlbs.EncodeBinary(payload)}
	if err := f.c.pending(ctx, *p); err != nil {
		return nil, nil, err
	}
	return f.raw, p, nil
}

func TestSyncAccessFreshImportUsesRegisterOnly(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c, base, _ := recoveryController(t, runnerCatalog(now))
	c.saved.Catalog = nil
	c.store = wlbs.CatalogStore{}
	cat := syncedTwoNodeCatalog(now)
	f := &freshRegisterClient{recoveryClient: base, raw: recoveryRaw(cat, "ok")}
	if err := c.synchronizeClientRound(context.Background(), f, false, 3, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.trace, []string{"register"}) || c.saved.Pending != nil || len(c.saved.Catalog.Nodes) != 2 || c.saved.Catalog.SlotsUsed != 1 {
		t.Fatal("fresh register invoked sync/new seat path", f.trace)
	}
}

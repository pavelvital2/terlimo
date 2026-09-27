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

type recoveryClient struct {
	t       *testing.T
	c       *managedController
	trace   []string
	refresh func(wlbs.RefreshPayload) ([]byte, error)
	sync    func(string) ([]byte, error)
	status  func(wlbs.PendingOperation) ([]byte, error)
	resume  func(wlbs.PendingOperation) ([]byte, error)
	catalog func() ([]byte, error)
}

func (f *recoveryClient) Register(context.Context) ([]byte, *wlbs.PendingOperation, error) {
	f.t.Fatal("recovery must never register or generate another installation")
	return nil, nil, errors.New("UNEXPECTED_REGISTER")
}
func (f *recoveryClient) Refresh(ctx context.Context, p wlbs.RefreshPayload) ([]byte, *wlbs.PendingOperation, error) {
	f.trace = append(f.trace, "refresh")
	id, _ := wlbs.NewID()
	raw, _ := json.Marshal(p)
	pending := &wlbs.PendingOperation{RequestID: id.String(), Op: "refresh_access", PayloadB64: wlbs.EncodeBinary(raw)}
	if e := f.c.pending(ctx, *pending); e != nil {
		return nil, nil, e
	}
	if f.refresh == nil {
		f.t.Fatal("unexpected refresh")
	}
	r, e := f.refresh(p)
	return r, pending, e
}
func (f *recoveryClient) SyncAccess(ctx context.Context, registration string) ([]byte, *wlbs.PendingOperation, error) {
	f.trace = append(f.trace, "sync_access")
	id, _ := wlbs.NewID()
	p := wlbs.SyncAccessPayload{Op: "sync_access", CredentialID: "fixture", PublicKeySPKI: "fixture", InstallationID: "fixture", RegistrationID: registration}
	raw, _ := json.Marshal(p)
	pending := &wlbs.PendingOperation{RequestID: id.String(), Op: "sync_access", PayloadB64: wlbs.EncodeBinary(raw)}
	if e := f.c.pending(ctx, *pending); e != nil {
		return nil, nil, e
	}
	if f.sync == nil {
		f.t.Fatal("unexpected sync_access")
	}
	r, e := f.sync(registration)
	return r, pending, e
}
func (f *recoveryClient) Status(_ context.Context, p wlbs.PendingOperation, registration string) ([]byte, error) {
	f.trace = append(f.trace, "status")
	if registration != "reg" || f.status == nil {
		f.t.Fatal("unexpected status")
	}
	return f.status(p)
}
func (f *recoveryClient) Resume(_ context.Context, p wlbs.PendingOperation) ([]byte, error) {
	f.trace = append(f.trace, "resume")
	if f.resume == nil {
		f.t.Fatal("unexpected resume")
	}
	return f.resume(p)
}
func (f *recoveryClient) Catalog(context.Context, string, string) ([]byte, error) {
	f.trace = append(f.trace, "catalog")
	if f.catalog == nil {
		f.t.Fatal("unexpected catalog")
	}
	return f.catalog()
}

func recoveryController(t *testing.T, old *wlbs.Catalog) (*managedController, *recoveryClient, *bool) {
	t.Helper()
	ack := true
	var bridge *managedBridge
	bridge = newManagedBridge(runnerTestWriter(func(raw []byte) (int, error) {
		var m bridgeMessage
		if json.Unmarshal(raw, &m) != nil {
			t.Fatal("invalid bridge")
		}
		if m.string("type") == "persist" {
			if !ack {
				return 0, io.ErrClosedPipe
			}
			bridge.mu.Lock()
			ch := bridge.waiters[m.string("request_id")]
			bridge.mu.Unlock()
			ch <- bridgeMessage{"type": "persist_result"}
		}
		return len(raw), nil
	}), "test-attempt", func() {})
	c := &managedController{bridge: bridge, link: &wlbs.Link{SubscriptionRef: "sub"}, saved: managedSaved{Subscription: "sub", Installation: "unchanged-key-fingerprint", Catalog: old}}
	if e := restoreManagedCatalog(&c.store, old, "sub", time.Now()); e != nil {
		t.Fatal(e)
	}
	return c, &recoveryClient{t: t, c: c}, &ack
}
func recoverySelectedController(t *testing.T, old *wlbs.Catalog) (*managedController, *recoveryClient, *bool) {
	t.Helper()
	c, f, ack := recoveryController(t, old)
	if old != nil && len(old.Nodes) != 0 {
		c.saved.SelectedNodeID = old.Nodes[0].NodeID
	}
	return c, f, ack
}

func recoveryRaw(cat *wlbs.Catalog, status string) []byte {
	raw, _ := json.Marshal(map[string]any{"v": 1, "status": status, "catalog": cat})
	return raw
}
func recoveryFresh(now time.Time) *wlbs.Catalog {
	c := runnerCatalog(now)
	c.Revision = "9"
	c.Nodes[0].Access.LeaseSeq = "3"
	return c
}

func TestPreconnectExpiredGrantRecovery(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerCatalog(now.Add(-20 * time.Minute))
	c, f, _ := recoverySelectedController(t, old)
	f.refresh = func(p wlbs.RefreshPayload) ([]byte, error) {
		if p.RegistrationID != "reg" || p.GrantID != "grant" || p.NodeID != "test" || p.ExpectedGeneration != "1" || p.ExpectedLeaseSeq != "1" {
			t.Fatal("identity/version changed")
		}
		if c.store.Snapshot().Validate("sub", "reg", time.Now()) == nil {
			t.Fatal("expired metadata authorized VPN")
		}
		return recoveryRaw(recoveryFresh(now), "ok"), nil
	}
	if e := c.synchronizeClient(context.Background(), f, false, 3); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(f.trace, []string{"refresh"}) || c.saved.Pending != nil || c.saved.Installation != "unchanged-key-fingerprint" || c.store.Snapshot().Validate("sub", "reg", time.Now()) != nil {
		t.Fatal("recovery result")
	}
}

func TestPreconnectLostRefreshResumesExactPending(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c, f, _ := recoverySelectedController(t, runnerCatalog(now.Add(-20*time.Minute)))
	f.refresh = func(wlbs.RefreshPayload) ([]byte, error) { return nil, io.ErrUnexpectedEOF }
	if e := c.synchronizeClient(context.Background(), f, false, 3); e == nil {
		t.Fatal("lost reply accepted")
	}
	p := *c.saved.Pending
	f.status = func(got wlbs.PendingOperation) ([]byte, error) {
		if got != p {
			t.Fatal("pending changed")
		}
		return []byte(`{"v":1,"status":"unknown"}`), nil
	}
	f.resume = func(got wlbs.PendingOperation) ([]byte, error) {
		if got != p {
			t.Fatal("resume bytes changed")
		}
		return recoveryRaw(recoveryFresh(now), "ok"), nil
	}
	if e := c.synchronizeClient(context.Background(), f, false, 3); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(f.trace, []string{"refresh", "status", "resume"}) {
		t.Fatal(f.trace)
	}
}

func TestPreconnectConflictReconcilesBeforeNewMutation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c, f, _ := recoverySelectedController(t, runnerCatalog(now.Add(-20*time.Minute)))
	n := 0
	var first wlbs.PendingOperation
	f.refresh = func(p wlbs.RefreshPayload) ([]byte, error) {
		n++
		if n == 1 {
			first = *c.saved.Pending
			return nil, errors.New("LEASE_CONFLICT")
		}
		if p.ExpectedLeaseSeq != "2" || c.saved.Pending.RequestID == first.RequestID {
			t.Fatal("conflict reused ID or stale seq")
		}
		return recoveryRaw(recoveryFresh(now), "ok"), nil
	}
	f.status = func(p wlbs.PendingOperation) ([]byte, error) {
		if p != first {
			t.Fatal("status wrong operation")
		}
		return []byte(`{"v":1,"status":"failed","code":"LEASE_CONFLICT"}`), nil
	}
	f.catalog = func() ([]byte, error) {
		if c.saved.Pending != nil {
			t.Fatal("unresolved mutation replaced")
		}
		cat := runnerCatalog(now)
		cat.Revision = "8"
		cat.Nodes[0].Access.LeaseSeq = "2"
		cat.Nodes[0].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
		if cat.ValidateNode("sub", "reg", "test", now) == nil {
			t.Fatal("expired lease accepted for VPN")
		}
		return recoveryRaw(cat, "ok"), nil
	}
	if e := c.synchronizeClient(context.Background(), f, false, 3); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(f.trace, []string{"refresh", "status", "catalog", "refresh"}) {
		t.Fatal(f.trace)
	}
}

func TestPreconnectPendingStopsWithoutNewMutation(t *testing.T) {
	for _, code := range []string{"OPERATION_PENDING", "DEVICE_REVOKED", "SUBSCRIPTION_EXPIRED"} {
		t.Run(code, func(t *testing.T) {
			c, f, _ := recoverySelectedController(t, runnerCatalog(time.Now().Add(-20*time.Minute)))
			p := &wlbs.PendingOperation{RequestID: "original", Op: "refresh_access", PayloadB64: "unchanged"}
			c.saved.Pending = p
			f.status = func(wlbs.PendingOperation) ([]byte, error) { return nil, errors.New(code) }
			if e := c.synchronizeClient(context.Background(), f, false, 3); managedCode(e) != code {
				t.Fatal(e)
			}
			if c.saved.Pending != p || !reflect.DeepEqual(f.trace, []string{"status"}) {
				t.Fatal("pending lost or mutation sent")
			}
		})
	}
}

func TestPreconnectRejectsUnsafeCatalog(t *testing.T) {
	for _, kind := range []string{"subscription_expired", "revoked", "grant_changed", "generation_changed", "revision_rollback", "seq_rollback", "still_expired"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			old := runnerCatalog(now.Add(-20 * time.Minute))
			c, f, _ := recoverySelectedController(t, old)
			f.refresh = func(wlbs.RefreshPayload) ([]byte, error) {
				cat := recoveryFresh(now)
				switch kind {
				case "subscription_expired":
					return nil, errors.New("SUBSCRIPTION_EXPIRED")
				case "revoked":
					return nil, errors.New("GRANT_REVOKED")
				case "grant_changed":
					cat.Nodes[0].Access.GrantID = "replacement"
				case "generation_changed":
					cat.Nodes[0].Access.Generation = "2"
				case "revision_rollback":
					cat.Revision = "6"
				case "seq_rollback":
					cat.Nodes[0].Access.LeaseSeq = "0"
				case "still_expired":
					return recoveryRaw(old, "ok"), nil
				}
				return recoveryRaw(cat, "ok"), nil
			}
			if e := c.synchronizeClient(context.Background(), f, false, 3); e == nil {
				t.Fatal("unsafe catalog accepted")
			}
			if len(f.trace) > 3 || c.store.Snapshot().Validate("sub", "reg", time.Now()) == nil {
				t.Fatal("unbounded retry or VPN allowed")
			}
		})
	}
}

func TestPreconnectPersistenceFailureKeepsPriorState(t *testing.T) {
	c, f, ack := recoverySelectedController(t, runnerCatalog(time.Now().Add(-20*time.Minute)))
	*ack = false
	f.refresh = func(wlbs.RefreshPayload) ([]byte, error) { t.Fatal("mutation before durable ACK"); return nil, nil }
	if e := c.synchronizeClient(context.Background(), f, false, 3); e == nil || c.saved.Pending != nil {
		t.Fatal("uncommitted pending retained", e)
	}
}

func TestPreconnectCompleteOldRegisterIsNotFreshAccess(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerCatalog(now.Add(-20 * time.Minute))
	c, f, _ := recoverySelectedController(t, old)
	c.saved.Pending = &wlbs.PendingOperation{RequestID: "old-register", Op: "register", PayloadB64: "original"}
	f.status = func(p wlbs.PendingOperation) ([]byte, error) {
		if p.Op != "register" {
			t.Fatal("pending rewritten")
		}
		return recoveryRaw(old, "complete"), nil
	}
	f.catalog = func() ([]byte, error) { return recoveryRaw(old, "ok"), nil }
	f.refresh = func(wlbs.RefreshPayload) ([]byte, error) {
		if c.store.Snapshot().Validate("sub", "reg", time.Now()) == nil {
			t.Fatal("status replay extended lease")
		}
		if c.saved.Pending.Op != "refresh_access" {
			t.Fatal("refresh missing durable operation")
		}
		return recoveryRaw(recoveryFresh(now), "ok"), nil
	}
	if e := c.synchronizeClient(context.Background(), f, false, 3); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(f.trace, []string{"status", "catalog", "refresh"}) {
		t.Fatal(f.trace)
	}
}

func TestPreconnectExpiredSubscriptionNoMutation(t *testing.T) {
	old := runnerCatalog(time.Now().Add(-2 * time.Hour))
	c, f, _ := recoverySelectedController(t, old)
	f.catalog = func() ([]byte, error) { return nil, errors.New("SUBSCRIPTION_EXPIRED") }
	if e := c.synchronizeClient(context.Background(), f, false, 3); managedCode(e) != "SUBSCRIPTION_EXPIRED" {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(f.trace, []string{"catalog"}) {
		t.Fatal("expired subscription mutated")
	}
}

func TestPreconnectRenewedSubscriptionRequiresAuthoritativeCatalog(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c, f, _ := recoverySelectedController(t, runnerCatalog(now.Add(-2*time.Hour)))
	f.catalog = func() ([]byte, error) { return recoveryRaw(recoveryFresh(now), "ok"), nil }
	if e := c.synchronizeClient(context.Background(), f, false, 3); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(f.trace, []string{"catalog"}) || c.store.Snapshot().Validate("sub", "reg", time.Now()) != nil {
		t.Fatal("renewed subscription recovery")
	}
}

func TestReconciledExpiredMetadataSurvivesRestartWithoutVPN(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cat := runnerCatalog(now)
	cat.Nodes[0].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	store := &wlbs.CatalogStore{}
	if e := restoreManagedCatalog(store, cat, "sub", now); e != nil {
		t.Fatal(e)
	}
	if store.Snapshot().ValidateNode("sub", "reg", "test", now) == nil {
		t.Fatal("metadata authorized VPN")
	}
}

func TestPendingIDCannotChangeBusinessPayload(t *testing.T) {
	c, _, _ := recoverySelectedController(t, runnerCatalog(time.Now()))
	p := wlbs.PendingOperation{RequestID: "original", Op: "refresh_access", PayloadB64: "bytes-before"}
	c.saved.Pending = &p
	changed := p
	changed.PayloadB64 = "bytes-after"
	if e := c.pending(context.Background(), changed); managedCode(e) != "OPERATION_PENDING" || *c.saved.Pending != p {
		t.Fatal("business bytes changed under ID", e)
	}
}

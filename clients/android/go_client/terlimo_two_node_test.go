package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"wg-turn-client/wlbs"
)

func runnerTwoNodes(now time.Time) *wlbs.Catalog {
	c := runnerCatalog(now)
	n := c.Nodes[0]
	n.NodeID, n.Name, n.PeerIP = "second", "Second", "192.0.2.2"
	n.Access.GrantID, n.Access.Password = "grant-second", "other-private"
	n.DTLSSPKISHA256 = wlbs.EncodeBinary(make([]byte, 32))
	c.Nodes = append(c.Nodes, n)
	return c
}

func TestTwoNodeUnselectedExpiredCatalogDoesNotRenewBoth(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerTwoNodes(now)
	for i := range old.Nodes {
		old.Nodes[i].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	}
	c, f, _ := recoveryController(t, old)
	f.catalog = func() ([]byte, error) { return recoveryRaw(old, "ok"), nil }
	if err := c.synchronizeClient(context.Background(), f, false, 3); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.trace, []string{"catalog"}) || c.saved.Pending != nil || c.saved.SelectedNodeID != "" {
		t.Fatal("unselected catalog mutated grants")
	}
	if c.store.Snapshot().Validate("sub", "reg", now) != nil || c.store.Snapshot().ValidateNode("sub", "reg", "test", now) == nil {
		t.Fatal("metadata/admission not separate")
	}
}

func TestTwoNodePreferenceQueueCoalescesWithoutBlockingCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newManagedBridge(io.Discard, "attempt", cancel)
	input := strings.Repeat("{\"v\":1,\"attempt_id\":\"attempt\",\"type\":\"choose_node\",\"node_id\":\"test\"}\n", 100)
	input += "{\"v\":1,\"attempt_id\":\"attempt\",\"type\":\"choose_node\",\"node_id\":\"second\"}\n{\"v\":1,\"attempt_id\":\"attempt\",\"type\":\"cancel\"}\n"
	done := make(chan struct{})
	go func() { b.read(ctx, bufio.NewScanner(strings.NewReader(input))); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("preference blocked cancel")
	}
	if ctx.Err() != context.Canceled || len(b.preference) != 1 || <-b.preference != "second" {
		t.Fatal("unbounded/stale preference")
	}
}

func TestActiveSwitchCommandIsExplicitAndBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newManagedBridge(io.Discard, "attempt", cancel)
	input := strings.Repeat("{\"v\":1,\"attempt_id\":\"attempt\",\"type\":\"switch_node\",\"node_id\":\"second\",\"switch_id\":\"11111111-1111-1111-1111-111111111111\",\"catalog_revision\":\"7\"}\n", 20)
	input += "{\"v\":1,\"attempt_id\":\"attempt\",\"type\":\"cancel\"}\n"
	done := make(chan struct{})
	go func() { b.read(ctx, bufio.NewScanner(strings.NewReader(input))); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("switch command blocked cancel")
	}
	if len(b.switchNode) != 1 || (<-b.switchNode) != (managedSwitch{ID: "11111111-1111-1111-1111-111111111111", NodeID: "second", Revision: "7"}) {
		t.Fatal("switch command was duplicated or changed")
	}
}

func TestActiveSwitchRejectsMalformedAndSameTargetOperations(t *testing.T) {
	good := managedSwitch{ID: "11111111-1111-1111-1111-111111111111", NodeID: "second", Revision: "7"}
	if !validManagedSwitch(good) {
		t.Fatal("valid operation rejected")
	}
	for _, bad := range []managedSwitch{
		{ID: "short", NodeID: good.NodeID, Revision: good.Revision},
		{ID: good.ID, NodeID: "", Revision: good.Revision},
		{ID: good.ID, NodeID: good.NodeID, Revision: ""},
	} {
		if validManagedSwitch(bad) {
			t.Fatal("malformed operation accepted")
		}
	}
}

func TestTwoNodeSelectedRefreshIgnoresOtherExpired(t *testing.T) {
	for _, bothExpired := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-selected", true: "both-expired"}[bothExpired], func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			old := runnerTwoNodes(now)
			old.Nodes[0].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
			if bothExpired {
				old.Nodes[1].Access.ExpiresAt = old.Nodes[0].Access.ExpiresAt
			}
			c, f, _ := recoveryController(t, old)
			c.saved.SelectedNodeID = "second"
			next := runnerTwoNodes(now)
			next.Revision = "8"
			next.Nodes[0].Access.ExpiresAt = old.Nodes[0].Access.ExpiresAt
			next.Nodes[1].Access.LeaseSeq = "2"
			f.refresh = func(p wlbs.RefreshPayload) ([]byte, error) {
				if p.NodeID != "second" || p.GrantID != "grant-second" || p.ExpectedLeaseSeq != "1" {
					t.Fatal("wrong selected refresh")
				}
				return recoveryRaw(next, "ok"), nil
			}
			f.catalog = func() ([]byte, error) { return recoveryRaw(old, "ok"), nil }
			if err := c.synchronizeClient(context.Background(), f, false, 3); err != nil {
				t.Fatal(err)
			}
			want := []string{"catalog"}
			if bothExpired {
				want = []string{"refresh"}
			}
			if !reflect.DeepEqual(want, f.trace) || c.saved.SelectedNodeID != "second" || c.saved.Pending != nil || c.saved.Installation != "unchanged-key-fingerprint" {
				t.Fatal("extra operation/identity change", f.trace)
			}
			cat := c.store.Snapshot()
			if cat.ValidateNode("sub", "reg", "second", now) != nil || cat.ValidateNode("sub", "reg", "test", now) == nil || cat.SlotsUsed != 1 || len(cat.Nodes) != 2 {
				t.Fatal("admission or seat boundary")
			}
		})
	}
}

func TestTwoNodeLostRefreshCompletionDurableBeforeAdmission(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerTwoNodes(now)
	for i := range old.Nodes {
		old.Nodes[i].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	}
	c, f, ack := recoveryController(t, old)
	c.saved.SelectedNodeID = "second"
	f.refresh = func(wlbs.RefreshPayload) ([]byte, error) { return nil, io.ErrUnexpectedEOF }
	if c.synchronizeClient(context.Background(), f, false, 3) == nil || c.saved.Pending == nil {
		t.Fatal("missing exact pending")
	}
	p := *c.saved.Pending
	next := runnerTwoNodes(now)
	next.Revision = "8"
	next.Nodes[0].Access.ExpiresAt = old.Nodes[0].Access.ExpiresAt
	next.Nodes[1].Access.LeaseSeq = "2"
	f.status = func(got wlbs.PendingOperation) ([]byte, error) {
		if got != p {
			t.Fatal("changed id/payload")
		}
		return recoveryRaw(next, "complete"), nil
	}
	*ack = false
	if c.synchronizeClient(context.Background(), f, false, 3) == nil || *c.saved.Pending != p || c.saved.Catalog.Revision != "7" || c.store.Snapshot().ValidateNode("sub", "reg", "second", now) == nil {
		t.Fatal("failed durable write exposed admission/cleared pending")
	}
	*ack = true
	if err := c.synchronizeClient(context.Background(), f, false, 3); err != nil {
		t.Fatal(err)
	}
	if c.saved.Pending != nil || c.saved.SelectedNodeID != "second" || c.saved.Catalog.Revision != "8" || c.store.Snapshot().ValidateNode("sub", "reg", "second", now) != nil || !reflect.DeepEqual(f.trace, []string{"refresh", "status", "status"}) {
		t.Fatal("not idempotent completion", f.trace)
	}
	// Simulate process restart from the exact encrypted-persistence payload.
	raw, _ := json.Marshal(c.saved)
	var saved managedSaved
	if json.Unmarshal(raw, &saved) != nil || saved.SelectedNodeID != "second" || saved.Pending != nil || saved.Catalog.Nodes[0].Access.ExpiresAt != old.Nodes[0].Access.ExpiresAt {
		t.Fatal("whole result not retained")
	}
}

func TestTwoNodeSelectionDurableAndNeverFallsBack(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c, _, ack := recoveryController(t, runnerTwoNodes(now))
	if err := c.chooseNode(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	*ack = false
	if c.chooseNode(context.Background(), "test") == nil || c.saved.SelectedNodeID != "second" {
		t.Fatal("selection changed before persist ACK")
	}
	*ack = true
	next := runnerTwoNodes(now)
	next.Revision = "8"
	next.Nodes = []wlbs.Node{next.Nodes[0]}
	if sameManagedGrant(c.saved.Catalog, next) != nil || c.store.Apply(next, "sub", "reg", now) != nil {
		t.Fatal("removal metadata")
	}
	if id := managedSelectionID(next, c.saved.SelectedNodeID); id != "second" {
		t.Fatal("silent default", id)
	}
	if _, err := managedNodeByID(c.store.Snapshot(), c.saved.SelectedNodeID); managedCode(err) != "SELECTED_NODE_REMOVED" {
		t.Fatal("removed selection substituted", err)
	}
	if err := c.chooseNode(context.Background(), "test"); err != nil || c.saved.SelectedNodeID != "test" {
		t.Fatal("explicit replacement failed", err)
	}
	if managedSelectionID(runnerTwoNodes(now), "") != "" || managedSelectionID(runnerCatalog(now), "") != "" {
		t.Fatal("empty selection became an implicit default")
	}
}

func TestTwoNodeImmutableOldStatusSavedBeforeCatalogQuery(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerTwoNodes(now)
	old.Revision = "8"
	c, f, ack := recoveryController(t, old)
	c.saved.SelectedNodeID = "second"
	p := wlbs.PendingOperation{RequestID: "exact-original", Op: "refresh_access", PayloadB64: "exact-bytes"}
	c.saved.Pending = &p
	completed := runnerTwoNodes(now.Add(-20 * time.Minute))
	// Immutable old result may be older than the saved metadata. Do not
	// overwrite the live revision with it, but retain it as a bounded receipt.
	f.status = func(got wlbs.PendingOperation) ([]byte, error) {
		if got != p {
			t.Fatal("pending identity changed")
		}
		return recoveryRaw(completed, "complete"), nil
	}
	f.catalog = func() ([]byte, error) {
		if c.saved.Completed == nil || c.saved.Completed.RequestID != p.RequestID ||
			!reflect.DeepEqual(c.saved.Completed.Catalog, completed) || c.saved.Pending == nil || *c.saved.Pending != p || c.saved.SelectedNodeID != "second" {
			t.Fatal("metadata query before durable exact completion")
		}
		if c.store.Snapshot().Revision != "8" {
			t.Fatal("historical completion rolled back live version")
		}
		return recoveryRaw(old, "ok"), nil
	}
	*ack = false
	if c.synchronizeClient(context.Background(), f, false, 3) == nil || !reflect.DeepEqual(f.trace, []string{"status"}) || *c.saved.Pending != p || c.saved.Completed != nil {
		t.Fatal("failed completion persist allowed query")
	}
	*ack = true
	if err := c.synchronizeClient(context.Background(), f, false, 3); err != nil {
		t.Fatal(err)
	}
	if c.saved.Pending != nil || c.saved.Catalog.Revision != "8" || c.saved.Completed.Catalog.Revision != "7" || !reflect.DeepEqual(f.trace, []string{"status", "status", "catalog"}) {
		t.Fatal("completion silently replaced by current metadata", f.trace)
	}
}

func TestTwoNodeCompletionReceiptRejectsChangedSameIDReplay(t *testing.T) {
	cat := runnerTwoNodes(time.Now().UTC().Truncate(time.Second))
	c, _, ack := recoveryController(t, cat)
	c.saved.SelectedNodeID = "second"
	p := wlbs.PendingOperation{RequestID: "same-id", Op: "refresh_access", PayloadB64: "original"}
	c.saved.Pending = &p
	if err := c.recordCompletion(context.Background(), p.RequestID, cat); err != nil {
		t.Fatal(err)
	}
	*ack = false // identical replay must use the already durable receipt, no rewrite.
	if err := c.recordCompletion(context.Background(), p.RequestID, cat); err != nil {
		t.Fatal(err)
	}
	changed := runnerTwoNodes(time.Now().UTC().Truncate(time.Second))
	changed.Revision = "8"
	if err := c.recordCompletion(context.Background(), p.RequestID, changed); managedCode(err) != "REVISION_CONFLICT" {
		t.Fatal("same-ID completion rewritten", err)
	}
	if !reflect.DeepEqual(c.saved.Completed.Catalog, cat) || *c.saved.Pending != p || c.saved.SelectedNodeID != "second" {
		t.Fatal("replay altered durable state")
	}
}

func TestTwoNodeGrantBindingAndVersionFences(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, kind := range []string{"swapped-grants", "generation", "wrong-grant"} {
		t.Run(kind, func(t *testing.T) {
			old, next := runnerTwoNodes(now), runnerTwoNodes(now)
			next.Revision = "8"
			switch kind {
			case "swapped-grants":
				next.Nodes[0].Access.GrantID, next.Nodes[1].Access.GrantID = next.Nodes[1].Access.GrantID, next.Nodes[0].Access.GrantID
			case "generation":
				next.Nodes[1].Access.Generation = "2"
			case "wrong-grant":
				next.Nodes[1].Access.GrantID = "other"
			}
			if managedCode(sameManagedGrant(old, next)) != "GRANT_REVOKED" {
				t.Fatal("node/grant fence bypass")
			}
		})
	}
	old, next := runnerTwoNodes(now), runnerTwoNodes(now)
	next.Revision = "8"
	next.Nodes[0], next.Nodes[1] = next.Nodes[1], next.Nodes[0]
	if sameManagedGrant(old, next) != nil {
		t.Fatal("reorder rejected")
	}
	n, err := managedNodeByID(next, "second")
	if err != nil || n.Access.GrantID != "grant-second" {
		t.Fatal("index selected")
	}
}

func TestActiveSwitchAdmissionSnapshotFence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cat := runnerTwoNodes(now)
	target := cat.Nodes[1]
	if !sameManagedAdmission(cat, cat.Revision, target) {
		t.Fatal("exact admission rejected")
	}
	for name, mutate := range map[string]func(*wlbs.Catalog){
		"revision":   func(next *wlbs.Catalog) { next.Revision = "9" },
		"generation": func(next *wlbs.Catalog) { next.Nodes[1].Access.Generation = "2" },
		"grant":      func(next *wlbs.Catalog) { next.Nodes[1].Access.GrantID = "changed" },
		"pin": func(next *wlbs.Catalog) {
			next.Nodes[1].DTLSSPKISHA256 = wlbs.EncodeBinary([]byte("01234567890123456789012345678901"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			next := runnerTwoNodes(now)
			mutate(next)
			if sameManagedAdmission(next, cat.Revision, target) {
				t.Fatal("stale completion accepted")
			}
		})
	}
}

func TestActiveSwitchRollbackRequiresCurrentEligibleExactGrant(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cat := runnerTwoNodes(now)
	c := &managedController{link: &wlbs.Link{SubscriptionRef: "sub"}}
	if err := restoreManagedCatalog(&c.store, cat, "sub", now); err != nil {
		t.Fatal(err)
	}
	old := cat.Nodes[0]
	if !c.rollbackAllowed(old, "reg") {
		t.Fatal("current eligible last-good rejected")
	}
	for name, mutate := range map[string]func(*wlbs.Catalog){
		"generation": func(next *wlbs.Catalog) { next.Nodes[0].Access.Generation = "2" },
		"grant":      func(next *wlbs.Catalog) { next.Nodes[0].Access.GrantID = "changed" },
		"pin": func(next *wlbs.Catalog) {
			next.Nodes[0].DTLSSPKISHA256 = wlbs.EncodeBinary([]byte("01234567890123456789012345678901"))
		},
		"expired": func(next *wlbs.Catalog) { next.Nodes[0].Access.ExpiresAt = now.Add(-time.Second).Format(time.RFC3339) },
	} {
		t.Run(name, func(t *testing.T) {
			next := runnerTwoNodes(now)
			mutate(next)
			c.store = wlbs.CatalogStore{}
			if err := restoreManagedCatalog(&c.store, next, "sub", now); err != nil && name != "expired" {
				t.Fatal(err)
			}
			if c.rollbackAllowed(old, "reg") {
				t.Fatal("unsafe rollback accepted")
			}
		})
	}
}

func TestActiveSwitchRepeatedOwnershipDoesNotChainCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	terminal := make(chan error, 1)
	for i := 0; i < 128; i++ {
		owned := make(chan struct{})
		close(owned)
		done := make(chan error, 1)
		finishManagedOwner(ctx, owned, done, terminal, errManagedRuntimeHandedOff)
		if len(done) != 0 || len(terminal) != 0 {
			t.Fatal("successful ownership transfer accumulated a completion")
		}
	}
	owned := make(chan struct{})
	done := make(chan error, 1)
	want := errors.New("VPN_SETUP_FAILED")
	finishManagedOwner(ctx, owned, done, terminal, want)
	if <-done != want {
		t.Fatal("pre-handoff failure lost")
	}
	close(owned)
	finishManagedOwner(ctx, owned, done, terminal, want)
	if <-terminal != want {
		t.Fatal("active owner terminal failure lost")
	}
}

func TestTwoNodeProbeMappingHasNoGlobalFallback(t *testing.T) {
	c := &managedController{start: managedStart{ProbeURL: "https://legacy.invalid/", ExpectedExitIP: "192.0.2.3", ProbeByNode: map[string]managedProbe{"test": {ProbeURL: "https://probe.invalid/", ExpectedExitIP: "192.0.2.3"}}}}
	if _, err := c.probeForNode("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.probeForNode("second"); managedCode(err) != "PROBE_NOT_PROVISIONED" {
		t.Fatal("other/global probe reused")
	}
	for _, url := range []string{"http://probe.invalid/", "https://user@probe.invalid/", "https://probe.invalid/#fragment", ""} {
		c.start.ProbeByNode["second"] = managedProbe{ProbeURL: url, ExpectedExitIP: "192.0.2.3"}
		if _, err := c.probeForNode("second"); err == nil {
			t.Fatal("unsafe probe")
		}
	}
}

func TestManagedWorkerTransportPolicyUsesSignedHashFallbackAndCompatibleTURN(t *testing.T) {
	hashFallback, turnStreamFirst := managedWorkerTransportPolicy()
	if !hashFallback {
		t.Fatal("managed path disabled signed VK hash reserves")
	}
	if turnStreamFirst {
		t.Fatal("managed path changed compatible UDP-first TURN ordering")
	}
}

func TestTwoNodeWorkerProofBindsSelectedNodeGrantAndExporter(t *testing.T) {
	cat := runnerTwoNodes(time.Now())
	first, second := cat.Nodes[0], cat.Nodes[1]
	id := managedWorkerIdentity(second, "reg", strings.Repeat("a", 32), "getconf", 1)
	if id.NodeID != "second" || id.GrantID != "grant-second" || id.WorkerID != "0" || id.ValidateWorker(36) != nil {
		t.Fatal("incorrect production identity")
	}
	payload, _ := json.Marshal(id)
	exporter, challenge, nonce := make([]byte, 32), make([]byte, 16), make([]byte, 32)
	transcript, err := wlbs.VPNTranscript(exporter, challenge, nonce, payload)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	digest := sha256.Sum256(transcript)
	sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if wlbs.VerifyTranscript(pub, transcript, sig) != nil {
		t.Fatal("selected proof rejected")
	}
	for _, kind := range []string{"node", "grant", "exporter"} {
		altered := id
		ex := append([]byte(nil), exporter...)
		switch kind {
		case "node":
			altered.NodeID = first.NodeID
		case "grant":
			altered.GrantID = first.Access.GrantID
		case "exporter":
			ex[0] = 1
		}
		raw, _ := json.Marshal(altered)
		tt, _ := wlbs.VPNTranscript(ex, challenge, nonce, raw)
		if wlbs.VerifyTranscript(pub, tt, sig) == nil {
			t.Fatal("cross-node proof accepted", kind)
		}
	}
}

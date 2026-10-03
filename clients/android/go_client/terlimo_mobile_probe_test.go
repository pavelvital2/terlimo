package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"
	"wg-turn-client/accountaccess"
	"wg-turn-client/wlbs"
)

// Decoded fixture objects and fakeIO seams only: no HTTP server, VPN, device or disk.
func probeCatalogFixture(t *testing.T) (*managedController, *managedMobile, accountaccess.MeResponse, accountaccess.CatalogResponse, *feedHost) {
	t.Helper()
	f := newMobileFeedFixture(t, 2, mobileFeedOptions{})
	me, cat := feedDecodeMe(t, f.me), feedDecodeCatalog(t, f)
	host := &feedHost{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &managedController{start: managedStart{MobileBaseURL: "https://unused.invalid"}, saved: managedSaved{SelectedNodeID: "gw-0"}}
	c.bridge = newManagedBridge(host, "probe-attempt", cancel)
	m := &managedMobile{controller: c, bridge: c.bridge, fingerprint: f.fingerprint, runCtx: ctx, verifiedSig: make(chan struct{}, 1)}
	c.mobile = m
	m.onVerified(me, cat)
	if m.verifiedErr != nil {
		t.Fatal(m.verifiedErr)
	}
	t.Cleanup(m.retireProbeAdmission)
	return c, m, me, cat, host
}
func installProbeFakeIO(t *testing.T, echo func(context.Context, net.Conn) (time.Duration, error)) *int {
	t.Helper()
	oldConnect, oldEcho := managedProbeConnect, managedProbeEcho
	calls := new(int)
	managedProbeConnect = func(ctx context.Context, node wlbs.Node) (net.Conn, func(), error) {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if node.NodeID != "gw-1" {
			t.Errorf("dial wrong node %q", node.NodeID)
		}
		*calls++
		a, b := net.Pipe()
		return a, func() { a.Close(); b.Close() }, nil
	}
	managedProbeEcho = echo
	t.Cleanup(func() { managedProbeConnect, managedProbeEcho = oldConnect, oldEcho })
	return calls
}
func TestMobileCatalogProbeAdmission(t *testing.T) {
	t.Run("B_without_asset_keeps_A_store_and_disk", func(t *testing.T) {
		c, _, _, _, host := probeCatalogFixture(t)
		before, _ := json.Marshal(c.store.Snapshot())
		saved := c.saved
		if len(c.start.ProbeByNode) != 0 {
			t.Fatal("fixture accidentally has assets")
		}
		if c.store.Snapshot().ValidateNode("", "", "gw-1", time.Now()) == nil {
			t.Fatal("global proof must still be A-only")
		}
		calls := installProbeFakeIO(t, func(context.Context, net.Conn) (time.Duration, error) { return 37 * time.Millisecond, nil })
		got, err := c.runNodeProbe(context.Background(), "gw-1")
		if err != nil || *calls != 1 || got.RTT != 37*time.Millisecond {
			t.Fatalf("fakeIO B not reached: %v %+v calls=%d", err, got, *calls)
		}
		after, _ := json.Marshal(c.store.Snapshot())
		if string(before) != string(after) || !reflect.DeepEqual(saved, c.saved) || host.persists != 0 {
			t.Fatal("probe mutated store/selection/disk")
		}
		if c.store.Snapshot().ValidateNode("", "", "gw-0", time.Now()) != nil {
			t.Fatal("A proof changed")
		}
		t.Log("fakeIO B dial+echo; asset entries=0; A selection/store/proof unchanged; persist frames=0")
	})
	for _, kind := range []string{"expired", "removed", "rejected_projection", "pending", "browse", "revoked_me"} {
		t.Run(kind, func(t *testing.T) {
			c, m, me, cat, _ := probeCatalogFixture(t)
			calls := installProbeFakeIO(t, func(context.Context, net.Conn) (time.Duration, error) { t.Error("unexpected echo"); return 0, nil })
			switch kind {
			case "expired":
				cat.ValidUntil = time.Now().Add(-time.Second).Format(time.RFC3339Nano)
				m.onVerified(me, cat)
			case "removed":
				cat.Gateways = cat.Gateways[:1]
				cat.Revision = "8"
				m.onVerified(me, cat)
			case "rejected_projection":
				cat.Gateways[1].TargetWorkers = nil
				m.onVerified(me, cat)
			case "pending":
				c.saved.Pending = &wlbs.PendingOperation{}
			case "browse":
				m.onBrowse(accountaccess.BrowseCatalogResponse{})
			case "revoked_me":
				me.AccountState = "REVOKED_SESSION"
				m.onCurrentMe(me)
			}
			if _, err := c.runNodeProbe(context.Background(), "gw-1"); err == nil || *calls != 0 {
				t.Fatalf("unsafe admission: %v, dials=%d", err, *calls)
			}
		})
	}
	t.Run("intermediate_pair_not_admitted_before_store_commit", func(t *testing.T) {
		c, m, me, cat, _ := probeCatalogFixture(t)
		m.mu.Lock()
		before := m.verified
		m.mu.Unlock()
		c.mu.Lock() // onVerified records acquisition fields, then blocks at selectionID.
		done := make(chan struct{})
		go func() { m.onVerified(me, cat); close(done) }()
		acquired := false
		until := time.Now().Add(time.Second)
		for time.Now().Before(until) {
			m.mu.Lock()
			acquired = m.verified > before
			m.mu.Unlock()
			if acquired {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !acquired {
			c.mu.Unlock()
			t.Fatal("candidate not observed")
		}
		_, err := m.admitProbe(context.Background(), "gw-1")
		c.mu.Unlock()
		<-done
		if err == nil {
			t.Fatal("uncommitted acquisition pair admitted")
		}
		lease, err := m.admitProbe(context.Background(), "gw-1")
		if err != nil {
			t.Fatal(err)
		}
		lease.close()
	})
	for _, kind := range []string{"same_revision_reissue", "revocation", "attempt_cancel", "session_change", "deadline"} {
		t.Run("late_"+kind, func(t *testing.T) {
			c, m, me, cat, _ := probeCatalogFixture(t)
			entered := make(chan context.Context, 1)
			release := make(chan struct{})
			installProbeFakeIO(t, func(ctx context.Context, _ net.Conn) (time.Duration, error) {
				entered <- ctx
				<-release
				return 42 * time.Millisecond, nil
			})
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "deadline" {
				// Exercise the accepted catalog deadline, not merely a caller timeout.
				cat.IssuedAt = time.Now().UTC().Format(time.RFC3339Nano)
				cat.ValidUntil = time.Now().Add(250 * time.Millisecond).UTC().Format(time.RFC3339Nano)
				m.onVerified(me, cat)
				if m.verifiedErr != nil {
					t.Fatal(m.verifiedErr)
				}
			}
			type outcome struct {
				measurement managedProbeMeasurement
				err         error
			}
			done := make(chan outcome, 1)
			go func() { v, e := c.runNodeProbe(parent, "gw-1"); done <- outcome{v, e} }()
			probeCtx := <-entered
			switch kind {
			case "same_revision_reissue":
				issued, _ := time.Parse(time.RFC3339Nano, cat.IssuedAt)
				cat.IssuedAt = issued.Add(time.Second).Format(time.RFC3339Nano)
				m.onVerified(me, cat)
			case "revocation":
				me.AccountState = "REVOKED_SESSION"
				m.onCurrentMe(me)
			case "attempt_cancel":
				cancel()
			case "session_change":
				m.session = &accountaccess.MobileSession{} // generation unavailable; no network.
			}
			if kind != "session_change" {
				select {
				case <-probeCtx.Done():
				case <-time.After(time.Second):
					close(release)
					t.Fatal("old probe context not canceled")
				}
			}
			close(release)
			result := <-done
			if result.err == nil || result.measurement.RTT != 0 {
				t.Fatal("late fake success survived", result)
			}
			frame := managedProbeResult("gw-1", result.measurement, result.err)
			if frame["status"] == "ok" || frame["rtt_ms"] != nil {
				t.Fatal("stale RTT published", frame)
			}
			if kind != "deadline" && !errors.Is(result.err, context.Canceled) {
				t.Fatal(result.err)
			}
			if c.selectionID() != "gw-0" {
				t.Fatal("A changed")
			}
		})
	}
}

func TestMobileCatalogProbeAfterSelectedRemoval(t *testing.T) {
	c, m, me, catalog, host := probeCatalogFixture(t)
	oldLease, err := m.admitProbe(context.Background(), "gw-0")
	if err != nil {
		t.Fatal(err)
	}
	defer oldLease.close()
	stops := 0
	release := c.registerMobileStop(func() { stops++ }, &managedDiagnostics{})
	defer release()
	catalog.Revision = "8"
	catalog.Gateways = catalog.Gateways[1:] // A removed; the new accepted pair still admits B.
	m.onVerified(me, catalog)
	if m.verifiedErr != nil {
		t.Fatalf("replacement was not committed: %v", m.verifiedErr)
	}
	stored := c.store.Snapshot()
	if len(stored.Nodes) != 1 || stored.Nodes[0].NodeID != "gw-1" || stored.Revision != "8" {
		t.Fatal("wrong committed catalog")
	}
	if c.selectionID() != "" || stops != 1 {
		t.Fatalf("A removal did not clear selection/stop old data plane: selection=%q stops=%d", c.selectionID(), stops)
	}
	if oldLease.check() == nil {
		t.Fatal("removed A lease survived")
	}
	before, _ := json.Marshal(stored)
	saved, persists := c.saved, host.persists
	calls := installProbeFakeIO(t, func(context.Context, net.Conn) (time.Duration, error) { return 41 * time.Millisecond, nil })
	got, err := c.runNodeProbe(context.Background(), "gw-1") // No intervening refresh or selection.
	if err != nil || *calls != 1 || got.RTT != 41*time.Millisecond {
		t.Fatalf("B immediately after A removal: err=%v dials=%d fakeRTT=%v", err, *calls, got.RTT)
	}
	after, _ := json.Marshal(c.store.Snapshot())
	if string(before) != string(after) || !reflect.DeepEqual(saved, c.saved) || persists != host.persists || stops != 1 {
		t.Fatal("ping changed committed store/selection/persistence or stopped VPN again")
	}
	t.Log("A removed: store committed B; A cleared; old runtime stop=1; old probe retired; B fakeIO dial+echo before any refresh; ping mutations=0")
}

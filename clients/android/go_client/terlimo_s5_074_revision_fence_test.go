package main

// S5-074: after the same-revision catalog freshness fix and connected renewal, a
// concurrent refresh may re-issue the same signed revision with a refreshed node lease
// (Access.ExpiresAt only) while a Connect/switch is already in progress holding a node
// snapshot captured before that refresh. The admission fence must compare material node
// identity/config only; lease shortening or extension stays enforced by DecideAdmission
// and the proven deadline. Material identity/config changes must still fail closed.

import (
	"testing"
	"time"

	"wg-turn-client/wlbs"
)

func TestSameManagedAdmissionAcceptsFreshnessOnlyLeaseChange(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cat := runnerTwoNodes(now)
	target := cat.Nodes[1]
	for name, shift := range map[string]time.Duration{
		"extended":  30 * time.Minute,
		"shortened": -time.Minute,
	} {
		t.Run(name, func(t *testing.T) {
			next := runnerTwoNodes(now)
			lease, err := wlbs.UTC(next.Nodes[1].Access.ExpiresAt)
			if err != nil {
				t.Fatal(err)
			}
			next.Nodes[1].Access.ExpiresAt = lease.Add(shift).UTC().Format(time.RFC3339Nano)
			if next.Nodes[1].Access.ExpiresAt == target.Access.ExpiresAt {
				t.Fatal("test mutation must change the lease literal")
			}
			if !sameManagedAdmission(next, cat.Revision, target) {
				t.Fatalf("%s lease must not be a revision conflict", name)
			}
			latest, err := managedNodeByID(next, target.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			if !sameManagedNodeConfig(latest, target) {
				t.Fatalf("%s lease changed a material field", name)
			}
		})
	}
}

func TestSameManagedAdmissionRejectsMaterialNodeChanges(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cat := runnerTwoNodes(now)
	target := cat.Nodes[1]
	material := map[string]func(*wlbs.Catalog){
		"grant":      func(next *wlbs.Catalog) { next.Nodes[1].Access.GrantID = "grant-changed" },
		"device":     func(next *wlbs.Catalog) { next.Nodes[1].Access.DeviceID = "device-changed" },
		"password":   func(next *wlbs.Catalog) { next.Nodes[1].Access.Password = "password-changed" },
		"vk_hashes":  func(next *wlbs.Catalog) { next.Nodes[1].Access.VKHashes = []string{"other-hash"} },
		"generation": func(next *wlbs.Catalog) { next.Nodes[1].Access.Generation = "2" },
		"lease_seq":  func(next *wlbs.Catalog) { next.Nodes[1].Access.LeaseSeq = "2" },
		"unlimited":  func(next *wlbs.Catalog) { next.Nodes[1].Access.Unlimited = true },
		"peer_ip":    func(next *wlbs.Catalog) { next.Nodes[1].PeerIP = "192.0.2.9" },
		"dtls_port":  func(next *wlbs.Catalog) { next.Nodes[1].DTLSPort = 444 },
		"dtls_pin": func(next *wlbs.Catalog) {
			next.Nodes[1].DTLSSPKISHA256 = wlbs.EncodeBinary([]byte("01234567890123456789012345678901"))
		},
		"wg_port":   func(next *wlbs.Catalog) { next.Nodes[1].WGPort = 51821 },
		"protocol":  func(next *wlbs.Catalog) { next.Nodes[1].Protocol = "wdtt-v18" },
		"auth_mode": func(next *wlbs.Catalog) { next.Nodes[1].AuthMode = "legacy-pop" },
		"workers":   func(next *wlbs.Catalog) { next.Nodes[1].MaxWorkers = 35 },
		"name":      func(next *wlbs.Catalog) { next.Nodes[1].Name = "Renamed" },
		"country":   func(next *wlbs.Catalog) { next.Nodes[1].CountryCode = "US" },
		"node_id":   func(next *wlbs.Catalog) { next.Nodes[1].NodeID = "third" },
		"revision":  func(next *wlbs.Catalog) { next.Revision = "8" },
	}
	for name, mutate := range material {
		t.Run(name, func(t *testing.T) {
			next := runnerTwoNodes(now)
			// A concurrent freshness-only refresh must never mask the material change.
			next.Nodes[1].Access.ExpiresAt = now.Add(time.Hour).UTC().Format(time.RFC3339Nano)
			mutate(next)
			if sameManagedAdmission(next, cat.Revision, target) {
				t.Fatalf("%s change must fail the admission fence", name)
			}
		})
	}
}

// TestMobileAdmissionFenceSameRevisionLeaseRefresh models the connect/switch timeline:
// the runner captured an admission node from the live store snapshot, then a
// same-revision refresh with a new lease lands in the store before the fence is
// evaluated at the pre-tunnel and pre-commit points. Only Access.ExpiresAt moved.
func TestMobileAdmissionFenceSameRevisionLeaseRefresh(t *testing.T) {
	now := time.Now().UTC()
	type mutation func(gateways []map[string]any)
	refresh := func(t *testing.T, lease time.Duration, mutate mutation) (*managedController, wlbs.Node, string) {
		t.Helper()
		fixture := newMobileFeedFixture(t, 2, mobileFeedOptions{})
		me := feedDecodeMe(t, fixture.me)
		initial, err := projectMobileCatalog(me, feedDecodeCatalog(t, fixture), fixture.fingerprint, "gw-1", now)
		if err != nil {
			t.Fatal(err)
		}
		controller := &managedController{start: managedStart{MobileBaseURL: "https://mobile.invalid"}}
		mobile := &managedMobile{controller: controller, fingerprint: fixture.fingerprint}
		controller.mobile = mobile
		controller.saved.SelectedNodeID = "gw-1"
		if err = controller.store.Apply(initial, "", fixture.fingerprint, now); err != nil {
			t.Fatal(err)
		}
		admission, err := managedNodeByID(controller.store.Snapshot(), "gw-1")
		if err != nil {
			t.Fatal(err)
		}
		if !controller.mobileAdmissionFence(controller.store.Snapshot(), fixture.revision, admission) {
			t.Fatal("pre-refresh admission fence must pass")
		}
		gateways := fixture.gatewayBodies()
		notAfter := feedStamp(now.Add(lease))
		for _, gateway := range gateways {
			gateway["access"].(map[string]any)["not_after"] = notAfter
		}
		if mutate != nil {
			mutate(gateways)
		}
		fixture.setValidUntil(now.Add(lease))
		fixture.setCatalog(fixture.revision, gateways)
		refreshed, err := projectMobileCatalog(me, feedDecodeCatalog(t, fixture), fixture.fingerprint, "gw-1", now)
		if err != nil {
			t.Fatal(err)
		}
		// The refresh path installs this projection into the live store in place. A
		// same-revision freshness/horizon refresh (lease, catalog validity, proof) is
		// accepted and retained (S5-074 store fix); a material same-revision change is
		// rejected by the store itself. The material fence scenario below is therefore
		// installed on an isolated store so the runner's defense-in-depth fence stays
		// covered independently of the store gate.
		err = controller.store.Apply(refreshed, "", fixture.fingerprint, now)
		if mutate == nil {
			if err != nil {
				t.Fatal(err)
			}
			stored := controller.store.Snapshot()
			if stored.CatalogExpiresAt != refreshed.CatalogExpiresAt ||
				stored.Nodes[1].Access.ExpiresAt != refreshed.Nodes[1].Access.ExpiresAt {
				t.Fatal("same-revision refresh was not retained by the live store")
			}
		} else {
			if managedCode(err) != "REVISION_CONFLICT" {
				t.Fatalf("material same-revision refresh must be rejected by the store: %v", err)
			}
			controller.store = wlbs.CatalogStore{}
			if err = controller.store.Apply(refreshed, "", fixture.fingerprint, now); err != nil {
				t.Fatal(err)
			}
		}
		return controller, admission, fixture.revision
	}

	t.Run("extended lease keeps admission", func(t *testing.T) {
		controller, admission, revision := refresh(t, 30*time.Minute, nil)
		// The pre-tunnel (terlimo_runner.go:1577) and pre-commit (:1626) fence points.
		if !controller.mobileAdmissionFence(controller.store.Snapshot(), revision, admission) {
			t.Fatal("an extended same-revision lease must not be a REVISION_CONFLICT")
		}
		if !controller.mobileAdmissionFence(controller.store.Snapshot(), revision, admission) {
			t.Fatal("the second fence point must agree")
		}
	})
	t.Run("shortened lease keeps admission", func(t *testing.T) {
		controller, admission, revision := refresh(t, 5*time.Minute, nil)
		if !controller.mobileAdmissionFence(controller.store.Snapshot(), revision, admission) {
			t.Fatal("a shortened same-revision lease is enforced by admission/deadline, not the fence")
		}
	})
	for name, mutate := range map[string]mutation{
		"generation": func(gateways []map[string]any) {
			gateways[1]["access"].(map[string]any)["generation"] = "2"
		},
		"password": func(gateways []map[string]any) {
			gateways[1]["access"].(map[string]any)["password"] = "password-changed"
		},
	} {
		t.Run("material "+name+" fails", func(t *testing.T) {
			controller, admission, revision := refresh(t, 30*time.Minute, mutate)
			if controller.mobileAdmissionFence(controller.store.Snapshot(), revision, admission) {
				t.Fatalf("a concurrent %s change must stay fail-closed", name)
			}
		})
	}
}

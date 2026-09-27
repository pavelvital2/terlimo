package wlbs

import (
	"testing"
	"time"
)

func twoNodeCatalog() Catalog {
	catalog := testCatalog()
	second := catalog.Nodes[0]
	second.NodeID = "test-2"
	second.PeerIP = "192.0.2.2"
	second.Access.GrantID = "grant-2"
	catalog.Nodes = append(catalog.Nodes, second)
	return catalog
}

func setCatalogTimes(catalog *Catalog, now time.Time) {
	catalog.IssuedAt = now.Add(-10 * time.Minute).Format(time.RFC3339)
	catalog.RefreshAfter = now.Add(-5 * time.Minute).Format(time.RFC3339)
	catalog.CatalogExpiresAt = now.Add(5 * time.Minute).Format(time.RFC3339)
	catalog.SubscriptionExpiresAt = now.Add(time.Hour).Format(time.RFC3339)
	for i := range catalog.Nodes {
		catalog.Nodes[i].Access.ExpiresAt = now.Add(5 * time.Minute).Format(time.RFC3339)
	}
}

func TestCatalogAcceptsTwoNodesWithoutConsumingAnotherSlot(t *testing.T) {
	catalog := twoNodeCatalog()

	if catalog.Nodes[0].Access.DeviceID != catalog.RegistrationID || catalog.Nodes[1].Access.DeviceID != catalog.RegistrationID {
		t.Fatal("nodes do not share the catalog installation")
	}
	if catalog.SlotsUsed != 1 || catalog.SlotsLimit != 2 {
		t.Fatalf("nodes unexpectedly changed seats: used=%d limit=%d", catalog.SlotsUsed, catalog.SlotsLimit)
	}
	if err := catalog.Validate("fixture-only", "registration", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogRejectsDuplicateNodeAndGrantIDs(t *testing.T) {
	for name, duplicate := range map[string]func(*Catalog){
		"node_id": func(catalog *Catalog) {
			catalog.Nodes[1].NodeID = catalog.Nodes[0].NodeID
		},
		"grant_id": func(catalog *Catalog) {
			catalog.Nodes[1].Access.GrantID = catalog.Nodes[0].Access.GrantID
		},
	} {
		t.Run(name, func(t *testing.T) {
			catalog := twoNodeCatalog()
			duplicate(&catalog)
			wireCode(t, catalog.Validate("fixture-only", "registration", time.Now()), "BAD_CATALOG")
		})
	}
}

func TestCatalogRejectsMoreThanTwoNodes(t *testing.T) {
	catalog := twoNodeCatalog()
	third := catalog.Nodes[1]
	third.NodeID = "test-3"
	third.PeerIP = "192.0.2.3"
	third.Access.GrantID = "grant-3"
	catalog.Nodes = append(catalog.Nodes, third)

	wireCode(t, catalog.Validate("fixture-only", "registration", time.Now()), "BAD_CATALOG")
}

func TestCatalogRejectsWrongPerNodeDeviceBindingAndMalformedPin(t *testing.T) {
	for name, malformed := range map[string]func(*Catalog){
		"device_binding": func(catalog *Catalog) {
			catalog.Nodes[1].Access.DeviceID = "another-registration"
		},
		"pin": func(catalog *Catalog) {
			catalog.Nodes[1].DTLSSPKISHA256 = "malformed"
		},
	} {
		t.Run(name, func(t *testing.T) {
			catalog := twoNodeCatalog()
			malformed(&catalog)
			wireCode(t, catalog.Validate("fixture-only", "registration", time.Now()), "BAD_CATALOG")
		})
	}
}

func TestCatalogRetainsTwoExpiredGrantsWithoutAdmission(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	catalog := twoNodeCatalog()
	setCatalogTimes(&catalog, now)
	for i := range catalog.Nodes {
		catalog.Nodes[i].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	}

	store := &CatalogStore{}
	if err := store.Apply(&catalog, "fixture-only", "registration", now); err != nil {
		t.Fatal(err)
	}
	if len(store.Snapshot().Nodes) != 2 {
		t.Fatal("expired grant metadata was discarded")
	}
	for _, node := range catalog.Nodes {
		wireCode(t, catalog.ValidateNode("fixture-only", "registration", node.NodeID, now), "BAD_CATALOG")
	}
}

func TestCatalogAdmitsOnlyFreshSelectedGrant(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	catalog := twoNodeCatalog()
	setCatalogTimes(&catalog, now)
	catalog.Nodes[0].Access.ExpiresAt = now.Add(5 * time.Minute).Format(time.RFC3339)
	catalog.Nodes[1].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)

	if err := catalog.Validate("fixture-only", "registration", now); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateNode("fixture-only", "registration", catalog.Nodes[0].NodeID, now); err != nil {
		t.Fatal(err)
	}
	wireCode(t, catalog.ValidateNode("fixture-only", "registration", catalog.Nodes[1].NodeID, now), "BAD_CATALOG")
	wireCode(t, catalog.ValidateNode("fixture-only", "registration", "removed-node", now), "SELECTED_NODE_REMOVED")
}

func TestCatalogNodeAdmissionRequiresFreshCatalogAndSubscription(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	t.Run("expired_catalog", func(t *testing.T) {
		catalog := twoNodeCatalog()
		setCatalogTimes(&catalog, now)
		catalog.CatalogExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
		catalog.Nodes[0].Access.ExpiresAt = now.Add(5 * time.Minute).Format(time.RFC3339)
		wireCode(t, catalog.ValidateNode("fixture-only", "registration", catalog.Nodes[0].NodeID, now), "BAD_CATALOG")
	})

	t.Run("expired_subscription", func(t *testing.T) {
		catalog := twoNodeCatalog()
		catalog.IssuedAt = now.Add(-20 * time.Minute).Format(time.RFC3339)
		catalog.RefreshAfter = now.Add(-15 * time.Minute).Format(time.RFC3339)
		catalog.CatalogExpiresAt = now.Add(-10 * time.Minute).Format(time.RFC3339)
		catalog.SubscriptionExpiresAt = now.Add(-5 * time.Minute).Format(time.RFC3339)
		for i := range catalog.Nodes {
			catalog.Nodes[i].Access.ExpiresAt = now.Add(-10 * time.Minute).Format(time.RFC3339)
		}
		wireCode(t, catalog.ValidateRefreshMetadata("fixture-only", "registration", now), "SUBSCRIPTION_EXPIRED")
		wireCode(t, catalog.ValidateNode("fixture-only", "registration", catalog.Nodes[0].NodeID, now), "BAD_CATALOG")
	})
}

func TestCatalogRefreshDeadlineMayEqualExpiry(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	catalog := twoNodeCatalog()
	setCatalogTimes(&catalog, now)
	catalog.RefreshAfter = catalog.CatalogExpiresAt

	if err := catalog.Validate("fixture-only", "registration", now); err != nil {
		t.Fatal(err)
	}
	catalog.RefreshAfter = now.Add(5*time.Minute + time.Second).Format(time.RFC3339)
	wireCode(t, catalog.Validate("fixture-only", "registration", now), "BAD_CATALOG")
}

func TestCatalogRefreshMetadataAcceptsExpiredCatalogAndGrants(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	catalog := twoNodeCatalog()
	setCatalogTimes(&catalog, now)
	catalog.CatalogExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	for i := range catalog.Nodes {
		catalog.Nodes[i].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	}

	if err := catalog.ValidateRefreshMetadata("fixture-only", "registration", now); err != nil {
		t.Fatal(err)
	}
	wireCode(t, catalog.Validate("fixture-only", "registration", now), "BAD_CATALOG")
}

func TestCatalogStoreHandlesTwoNodeReorderingByRevision(t *testing.T) {
	catalog := twoNodeCatalog()
	store := &CatalogStore{}
	apply := func(catalog *Catalog) error {
		return store.Apply(catalog, "fixture-only", "registration", time.Now())
	}
	if err := apply(&catalog); err != nil {
		t.Fatal(err)
	}

	higher := *cloneCatalog(&catalog)
	higher.Revision = "9007199254740994"
	higher.Nodes[0], higher.Nodes[1] = higher.Nodes[1], higher.Nodes[0]
	if err := apply(&higher); err != nil {
		t.Fatal(err)
	}

	conflict := *cloneCatalog(&higher)
	conflict.Nodes[0], conflict.Nodes[1] = conflict.Nodes[1], conflict.Nodes[0]
	wireCode(t, apply(&conflict), "REVISION_CONFLICT")
}

func TestCatalogStoreRejectsRollbackOnEitherNode(t *testing.T) {
	nodeNames := []string{"1", "2"}
	for _, field := range []string{"generation", "lease_seq"} {
		for node, nodeName := range nodeNames {
			t.Run(field+"_node_"+nodeName, func(t *testing.T) {
				catalog := twoNodeCatalog()
				catalog.Nodes[node].Access.Generation = "2"
				catalog.Nodes[node].Access.LeaseSeq = "2"
				store := &CatalogStore{}
				if err := store.Apply(&catalog, "fixture-only", "registration", time.Now()); err != nil {
					t.Fatal(err)
				}

				next := *cloneCatalog(&catalog)
				next.Revision = "9007199254740994"
				if field == "generation" {
					next.Nodes[node].Access.Generation = "1"
				} else {
					next.Nodes[node].Access.LeaseSeq = "1"
				}
				wireCode(t, store.Apply(&next, "fixture-only", "registration", time.Now()), "STALE_CATALOG")
			})
		}
	}
}

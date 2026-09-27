package wlbs

// S5-074 follow-up: a signed catalog revision is an identity fence, not a freshness
// fence. CatalogStore.Apply must accept a same-revision re-issue whose only changed
// literals are freshness/horizon fields (issued_at, refresh_after,
// catalog_expires_at, subscription_expires_at, server_time, per-node access.expires_at)
// plus the internal typed mobile proof, must retain the incoming snapshot so refreshed
// horizons are stored, and must still reject every material identity/config change and
// node-set/order change with REVISION_CONFLICT. A same-revision snapshot is installed
// only when its IssuedAt is equal to or strictly newer than the stored one; a strictly
// older (out-of-order) response is accepted but never installed, so it cannot re-extend
// a shortened catalog/subscription/node deadline. Shortened or expired horizons are
// stored as-is; admission and the proven deadline enforce them fail-closed elsewhere.

import (
	"fmt"
	"testing"
	"time"
)

func revisionFreshnessCatalog(now time.Time, nodes int) Catalog {
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	catalog := Catalog{
		V: 1, Status: "ok", SubscriptionRef: "fixture-only", RegistrationID: "registration",
		SubscriptionStatus: "active", SubscriptionExpiresAt: stamp(time.Hour),
		SlotsLimit: 2, SlotsUsed: 1, Revision: "9007199254740993",
		IssuedAt: stamp(-time.Minute), RefreshAfter: stamp(4 * time.Minute), CatalogExpiresAt: stamp(10 * time.Minute),
	}
	for index := 0; index < nodes; index++ {
		spki := make([]byte, 32)
		spki[0] = byte(index + 1)
		catalog.Nodes = append(catalog.Nodes, Node{
			Endpoint: Endpoint{
				NodeID:         fmt.Sprintf("test-%d", index+1),
				PeerIP:         fmt.Sprintf("192.0.2.%d", index+1),
				DTLSPort:       56000 + index,
				DTLSSPKISHA256: EncodeBinary(spki),
			},
			Name: fmt.Sprintf("TEST %d", index+1), CountryCode: "ZZ", WGPort: 51820 + index,
			Protocol: "wdtt-v17", AuthMode: "installation-pop-v1", MaxWorkers: 1,
			Access: Access{
				GrantID: fmt.Sprintf("grant-%d", index+1), DeviceID: "registration",
				Password:  "synthetic",
				VKHashes:  []string{fmt.Sprintf("synthetic-%d-a", index+1), fmt.Sprintf("synthetic-%d-b", index+1)},
				ExpiresAt: stamp(10 * time.Minute), Generation: "1", LeaseSeq: "1",
			},
		})
	}
	return catalog
}

func TestStoreSameRevisionAcceptsAndRetainsFreshnessEvolution(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	cases := []struct {
		name   string
		mutate func(*Catalog)
	}{
		{"lease extended", func(c *Catalog) { c.Nodes[0].Access.ExpiresAt = stamp(12 * time.Minute) }},
		{"lease shortened", func(c *Catalog) { c.Nodes[0].Access.ExpiresAt = stamp(30 * time.Second) }},
		{"lease expired", func(c *Catalog) { c.Nodes[0].Access.ExpiresAt = stamp(-2 * time.Minute) }},
		{"newer issued_at extended lease", func(c *Catalog) {
			c.IssuedAt = stamp(-30 * time.Second)
			c.Nodes[0].Access.ExpiresAt = stamp(12 * time.Minute)
		}},
		{"newer issued_at shortened lease", func(c *Catalog) {
			c.IssuedAt = stamp(-30 * time.Second)
			c.Nodes[0].Access.ExpiresAt = stamp(30 * time.Second)
		}},
		{"newer issued_at shortened catalog_expires_at", func(c *Catalog) {
			c.IssuedAt = stamp(-30 * time.Second)
			c.CatalogExpiresAt = stamp(5 * time.Minute)
		}},
		{"refresh_after moved", func(c *Catalog) { c.RefreshAfter = stamp(6 * time.Minute) }},
		{"catalog_expires_at extended", func(c *Catalog) { c.CatalogExpiresAt = stamp(12 * time.Minute) }},
		{"subscription_expires_at extended", func(c *Catalog) { c.SubscriptionExpiresAt = stamp(2 * time.Hour) }},
		{"server_time only", func(c *Catalog) { c.ServerTime = stamp(time.Second) }},
		{"all horizons refreshed", func(c *Catalog) {
			c.IssuedAt = stamp(0)
			c.RefreshAfter = stamp(5 * time.Minute)
			c.CatalogExpiresAt = stamp(8 * time.Minute)
			c.SubscriptionExpiresAt = stamp(2 * time.Hour)
			c.Nodes[0].Access.ExpiresAt = stamp(8 * time.Minute)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := revisionFreshnessCatalog(now, 1)
			next := *cloneCatalog(&base)
			tc.mutate(&next)
			store := &CatalogStore{}
			if err := store.Apply(&base, "fixture-only", "registration", now); err != nil {
				t.Fatal(err)
			}
			if err := store.Apply(&next, "fixture-only", "registration", now); err != nil {
				t.Fatalf("same-revision freshness evolution must be accepted: %v", err)
			}
			stored := store.Snapshot()
			if stored.IssuedAt != next.IssuedAt || stored.RefreshAfter != next.RefreshAfter ||
				stored.CatalogExpiresAt != next.CatalogExpiresAt || stored.SubscriptionExpiresAt != next.SubscriptionExpiresAt ||
				stored.ServerTime != next.ServerTime {
				t.Fatalf("catalog horizons were not retained: got %+v want %+v", stored, next)
			}
			for index := range next.Nodes {
				if stored.Nodes[index].Access.ExpiresAt != next.Nodes[index].Access.ExpiresAt {
					t.Fatalf("node %d lease was not retained: got %q want %q", index,
						stored.Nodes[index].Access.ExpiresAt, next.Nodes[index].Access.ExpiresAt)
				}
			}
			if err := stored.Validate("fixture-only", "registration", now); err != nil {
				t.Fatalf("stored snapshot must stay valid metadata: %v", err)
			}
		})
	}

	t.Run("earlier issued_at is accepted but never installed", func(t *testing.T) {
		base := revisionFreshnessCatalog(now, 1)
		store := &CatalogStore{}
		if err := store.Apply(&base, "fixture-only", "registration", now); err != nil {
			t.Fatal(err)
		}
		older := *cloneCatalog(&base)
		older.IssuedAt = stamp(-2 * time.Minute)
		older.CatalogExpiresAt = stamp(12 * time.Minute)
		older.Nodes[0].Access.ExpiresAt = stamp(12 * time.Minute)
		if err := store.Apply(&older, "fixture-only", "registration", now); err != nil {
			t.Fatalf("an order-rejected same-revision snapshot must not error: %v", err)
		}
		stored := store.Snapshot()
		if stored.IssuedAt != base.IssuedAt || stored.CatalogExpiresAt != base.CatalogExpiresAt ||
			stored.Nodes[0].Access.ExpiresAt != base.Nodes[0].Access.ExpiresAt {
			t.Fatalf("an older snapshot was installed: got %+v", stored)
		}
		if !sameRevisionIdentity(stored, &base) {
			t.Fatal("material identity changed on an order-rejected snapshot")
		}
	})

	t.Run("invalid issued_at keeps the stored snapshot", func(t *testing.T) {
		base := revisionFreshnessCatalog(now, 1)
		store := &CatalogStore{}
		if err := store.Apply(&base, "fixture-only", "registration", now); err != nil {
			t.Fatal(err)
		}
		// Only an internal caller can place a catalog that skipped Validate into
		// applyValidated; the fence must still fail safe instead of installing it.
		broken := *cloneCatalog(&base)
		broken.IssuedAt = "not-a-timestamp"
		if err := store.applyValidated(&broken); err != nil {
			t.Fatalf("unorderable snapshot must not error: %v", err)
		}
		if stored := store.Snapshot(); stored.IssuedAt != base.IssuedAt {
			t.Fatal("an unorderable snapshot replaced the stored one")
		}
	})

	t.Run("two nodes evolve independently", func(t *testing.T) {
		base := revisionFreshnessCatalog(now, 2)
		next := *cloneCatalog(&base)
		next.Nodes[0].Access.ExpiresAt = stamp(14 * time.Minute)
		next.Nodes[1].Access.ExpiresAt = stamp(30 * time.Second)
		store := &CatalogStore{}
		if err := store.Apply(&base, "fixture-only", "registration", now); err != nil {
			t.Fatal(err)
		}
		if err := store.Apply(&next, "fixture-only", "registration", now); err != nil {
			t.Fatalf("per-node freshness evolution must be accepted: %v", err)
		}
		stored := store.Snapshot()
		if stored.Nodes[0].Access.ExpiresAt != next.Nodes[0].Access.ExpiresAt ||
			stored.Nodes[1].Access.ExpiresAt != next.Nodes[1].Access.ExpiresAt {
			t.Fatal("per-node leases were not retained")
		}
	})

	t.Run("expired lease is retained but never admits", func(t *testing.T) {
		base := revisionFreshnessCatalog(now, 1)
		store := &CatalogStore{}
		if err := store.Apply(&base, "fixture-only", "registration", now); err != nil {
			t.Fatal(err)
		}
		next := *cloneCatalog(&base)
		next.Nodes[0].Access.ExpiresAt = stamp(-2 * time.Minute)
		if err := store.Apply(&next, "fixture-only", "registration", now); err != nil {
			t.Fatalf("an expired horizon is stored as-is, not widened: %v", err)
		}
		stored := store.Snapshot()
		if stored.Nodes[0].Access.ExpiresAt != next.Nodes[0].Access.ExpiresAt {
			t.Fatal("expired lease was not retained as-is")
		}
		if err := stored.Validate("fixture-only", "registration", now); err != nil {
			t.Fatalf("metadata validation must still pass: %v", err)
		}
		wireCode(t, stored.ValidateNode("fixture-only", "registration", "test-1", now), "BAD_CATALOG")
	})
}

func TestStoreSameRevisionRejectsMaterialChanges(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cases := []struct {
		name         string
		subscription string
		registration string
		mutate       func(*Catalog)
	}{
		{"password", "fixture-only", "registration", func(c *Catalog) { c.Nodes[0].Access.Password = "other" }},
		{"grant_id", "fixture-only", "registration", func(c *Catalog) { c.Nodes[0].Access.GrantID = "other-grant" }},
		{"generation increased", "fixture-only", "registration", func(c *Catalog) { c.Nodes[0].Access.Generation = "2" }},
		{"lease_seq increased", "fixture-only", "registration", func(c *Catalog) { c.Nodes[0].Access.LeaseSeq = "2" }},
		{"vk_hashes", "fixture-only", "registration", func(c *Catalog) { c.Nodes[0].Access.VKHashes = []string{"other"} }},
		{"vk_hashes reordered", "fixture-only", "registration", func(c *Catalog) {
			c.Nodes[0].Access.VKHashes = []string{"synthetic-1-b", "synthetic-1-a"}
		}},
		{"peer_ip", "fixture-only", "registration", func(c *Catalog) { c.Nodes[0].PeerIP = "198.51.100.7" }},
		{"dtls_port", "fixture-only", "registration", func(c *Catalog) { c.Nodes[0].DTLSPort = 56009 }},
		{"dtls_spki_sha256", "fixture-only", "registration", func(c *Catalog) {
			c.Nodes[0].DTLSSPKISHA256 = EncodeBinary([]byte("01234567890123456789012345678901"))
		}},
		{"wg_port", "fixture-only", "registration", func(c *Catalog) { c.Nodes[0].WGPort = 51829 }},
		{"max_workers", "fixture-only", "registration", func(c *Catalog) { c.Nodes[0].MaxWorkers = 2 }},
		{"name", "fixture-only", "registration", func(c *Catalog) { c.Nodes[0].Name = "Renamed" }},
		{"country_code", "fixture-only", "registration", func(c *Catalog) { c.Nodes[0].CountryCode = "US" }},
		{"node_id", "fixture-only", "registration", func(c *Catalog) { c.Nodes[0].NodeID = "other" }},
		{"slots_used", "fixture-only", "registration", func(c *Catalog) { c.SlotsUsed = 2 }},
		{"registration and device re-keyed", "fixture-only", "", func(c *Catalog) {
			c.RegistrationID = "other-registration"
			c.Nodes[0].Access.DeviceID = "other-registration"
		}},
		{"subscription_ref", "other-subscription", "", func(c *Catalog) { c.SubscriptionRef = "other-subscription" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := revisionFreshnessCatalog(now, 1)
			store := &CatalogStore{}
			if err := store.Apply(&base, "fixture-only", "registration", now); err != nil {
				t.Fatal(err)
			}
			next := *cloneCatalog(&base)
			tc.mutate(&next)
			wireCode(t, store.Apply(&next, tc.subscription, tc.registration, now), "REVISION_CONFLICT")
			if stored := store.Snapshot(); stored.Nodes[0].Access.Password != base.Nodes[0].Access.Password {
				t.Fatal("a rejected material change was stored")
			}
		})
	}

	t.Run("node removed", func(t *testing.T) {
		base := revisionFreshnessCatalog(now, 2)
		store := &CatalogStore{}
		if err := store.Apply(&base, "fixture-only", "registration", now); err != nil {
			t.Fatal(err)
		}
		next := *cloneCatalog(&base)
		next.Nodes = next.Nodes[:1]
		wireCode(t, store.Apply(&next, "fixture-only", "registration", now), "REVISION_CONFLICT")
	})
	t.Run("node added", func(t *testing.T) {
		base := revisionFreshnessCatalog(now, 1)
		store := &CatalogStore{}
		if err := store.Apply(&base, "fixture-only", "registration", now); err != nil {
			t.Fatal(err)
		}
		next := revisionFreshnessCatalog(now, 2)
		wireCode(t, store.Apply(&next, "fixture-only", "registration", now), "REVISION_CONFLICT")
	})
	t.Run("node order", func(t *testing.T) {
		base := revisionFreshnessCatalog(now, 2)
		store := &CatalogStore{}
		if err := store.Apply(&base, "fixture-only", "registration", now); err != nil {
			t.Fatal(err)
		}
		next := *cloneCatalog(&base)
		next.Nodes[0], next.Nodes[1] = next.Nodes[1], next.Nodes[0]
		wireCode(t, store.Apply(&next, "fixture-only", "registration", now), "REVISION_CONFLICT")
	})
}

// A same-revision generation/lease regression is caught by the earlier global grant
// fence in applyValidated and surfaces as STALE_CATALOG before the later
// LEASE_CONFLICT branch can be reached; both fences stay before the identity branch
// and the regression is never stored.
func TestStoreSameRevisionRejectsGenerationAndLeaseRegression(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for name, mutate := range map[string]func(*Catalog){
		"generation regression": func(c *Catalog) { c.Nodes[0].Access.Generation = "0" },
		"lease_seq regression":  func(c *Catalog) { c.Nodes[0].Access.LeaseSeq = "0" },
		"both regress together": func(c *Catalog) { c.Nodes[0].Access.Generation = "0"; c.Nodes[0].Access.LeaseSeq = "0" },
	} {
		t.Run(name, func(t *testing.T) {
			base := revisionFreshnessCatalog(now, 1)
			store := &CatalogStore{}
			if err := store.Apply(&base, "fixture-only", "registration", now); err != nil {
				t.Fatal(err)
			}
			next := *cloneCatalog(&base)
			mutate(&next)
			wireCode(t, store.Apply(&next, "fixture-only", "registration", now), "STALE_CATALOG")
			stored := store.Snapshot()
			if stored.Nodes[0].Access.Generation != base.Nodes[0].Access.Generation ||
				stored.Nodes[0].Access.LeaseSeq != base.Nodes[0].Access.LeaseSeq {
				t.Fatal("a rejected regression was stored")
			}
		})
	}
}

func TestSameRevisionIdentityFieldMatrix(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	base := revisionFreshnessCatalog(now, 2)
	if !sameRevisionIdentity(&base, &base) {
		t.Fatal("identity comparison must be reflexive")
	}
	if sameRevisionIdentity(nil, &base) || sameRevisionIdentity(&base, nil) || sameRevisionIdentity(nil, nil) {
		t.Fatal("nil catalogs must never be identical")
	}

	fresh := *cloneCatalog(&base)
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	fresh.IssuedAt = stamp(-2 * time.Minute)
	fresh.RefreshAfter = stamp(6 * time.Minute)
	fresh.CatalogExpiresAt = stamp(12 * time.Minute)
	fresh.SubscriptionExpiresAt = stamp(2 * time.Hour)
	fresh.ServerTime = stamp(time.Second)
	fresh.Nodes[0].Access.ExpiresAt = stamp(11 * time.Minute)
	fresh.Nodes[1].Access.ExpiresAt = stamp(30 * time.Second)
	if !sameRevisionIdentity(&fresh, &base) {
		t.Fatal("freshness/horizon literals must not be material identity")
	}

	atNode := func(index int, change func(*Node)) func(*Catalog) {
		return func(c *Catalog) { change(&c.Nodes[index]) }
	}
	material := map[string]func(*Catalog){
		"V":                      func(c *Catalog) { c.V = 2 },
		"status":                 func(c *Catalog) { c.Status = "complete" },
		"subscription_ref":       func(c *Catalog) { c.SubscriptionRef = "other" },
		"registration_id":        func(c *Catalog) { c.RegistrationID = "other" },
		"subscription_status":    func(c *Catalog) { c.SubscriptionStatus = "expired" },
		"subscription_unlimited": func(c *Catalog) { c.SubscriptionUnlimited = true },
		"slots_limit":            func(c *Catalog) { c.SlotsLimit = 3 },
		"slots_used":             func(c *Catalog) { c.SlotsUsed = 2 },
		"revision":               func(c *Catalog) { c.Revision = "8" },
		"node removed":           func(c *Catalog) { c.Nodes = c.Nodes[:1] },
		"node added":             func(c *Catalog) { c.Nodes = append(c.Nodes, c.Nodes[0]) },
		"node order":             func(c *Catalog) { c.Nodes[0], c.Nodes[1] = c.Nodes[1], c.Nodes[0] },
		"node_id":                atNode(0, func(n *Node) { n.NodeID = "other" }),
		"peer_ip":                atNode(0, func(n *Node) { n.PeerIP = "198.51.100.7" }),
		"dtls_port":              atNode(0, func(n *Node) { n.DTLSPort = 56009 }),
		"dtls_spki_sha256":       atNode(0, func(n *Node) { n.DTLSSPKISHA256 = EncodeBinary([]byte("01234567890123456789012345678901")) }),
		"name":                   atNode(0, func(n *Node) { n.Name = "Renamed" }),
		"country_code":           atNode(0, func(n *Node) { n.CountryCode = "US" }),
		"wg_port":                atNode(0, func(n *Node) { n.WGPort = 51829 }),
		"protocol":               atNode(0, func(n *Node) { n.Protocol = "wdtt-v18" }),
		"auth_mode":              atNode(0, func(n *Node) { n.AuthMode = "legacy-pop" }),
		"max_workers":            atNode(0, func(n *Node) { n.MaxWorkers = 2 }),
		"grant_id":               atNode(0, func(n *Node) { n.Access.GrantID = "other-grant" }),
		"device_id":              atNode(0, func(n *Node) { n.Access.DeviceID = "other-device" }),
		"password":               atNode(0, func(n *Node) { n.Access.Password = "other" }),
		"vk_hashes":              atNode(0, func(n *Node) { n.Access.VKHashes = []string{"other"} }),
		"vk_hashes order":        atNode(0, func(n *Node) { n.Access.VKHashes = []string{"synthetic-1-b", "synthetic-1-a"} }),
		"unlimited":              atNode(0, func(n *Node) { n.Access.Unlimited = true }),
		"generation":             atNode(0, func(n *Node) { n.Access.Generation = "2" }),
		"lease_seq":              atNode(0, func(n *Node) { n.Access.LeaseSeq = "2" }),
	}
	for name, change := range material {
		t.Run(name, func(t *testing.T) {
			next := *cloneCatalog(&base)
			change(&next)
			if sameRevisionIdentity(&next, &base) {
				t.Fatalf("%s must stay material identity", name)
			}
		})
	}
}

func TestStoreSameRevisionOlderIssuedAtCannotReextendShortenedDeadlines(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	base := revisionFreshnessCatalog(now, 1)
	store := &CatalogStore{}
	if err := store.Apply(&base, "fixture-only", "registration", now); err != nil {
		t.Fatal(err)
	}
	shortened := *cloneCatalog(&base)
	shortened.IssuedAt = stamp(0)
	shortened.CatalogExpiresAt = stamp(5 * time.Minute)
	shortened.SubscriptionExpiresAt = stamp(30 * time.Minute)
	shortened.Nodes[0].Access.ExpiresAt = stamp(30 * time.Second)
	if err := store.Apply(&shortened, "fixture-only", "registration", now); err != nil {
		t.Fatalf("a newer same-revision shortening must install: %v", err)
	}
	stored := store.Snapshot()
	if stored.CatalogExpiresAt != shortened.CatalogExpiresAt ||
		stored.SubscriptionExpiresAt != shortened.SubscriptionExpiresAt ||
		stored.Nodes[0].Access.ExpiresAt != shortened.Nodes[0].Access.ExpiresAt {
		t.Fatal("newer shortened horizons were not installed")
	}

	older := *cloneCatalog(&base)
	older.IssuedAt = stamp(-90 * time.Second)
	older.CatalogExpiresAt = stamp(12 * time.Minute)
	older.SubscriptionExpiresAt = stamp(2 * time.Hour)
	older.Nodes[0].Access.ExpiresAt = stamp(12 * time.Minute)
	if err := store.Apply(&older, "fixture-only", "registration", now); err != nil {
		t.Fatalf("an out-of-order older snapshot must be accepted without installing: %v", err)
	}
	stored = store.Snapshot()
	if stored.IssuedAt != shortened.IssuedAt || stored.CatalogExpiresAt != shortened.CatalogExpiresAt ||
		stored.SubscriptionExpiresAt != shortened.SubscriptionExpiresAt ||
		stored.Nodes[0].Access.ExpiresAt != shortened.Nodes[0].Access.ExpiresAt {
		t.Fatalf("an older snapshot re-extended shortened deadlines: got %+v want %+v", stored, shortened)
	}
	if !sameRevisionIdentity(stored, &shortened) {
		t.Fatal("material identity changed on the order-rejected snapshot")
	}
	if err := stored.Validate("fixture-only", "registration", now); err != nil {
		t.Fatalf("stored snapshot must stay valid metadata: %v", err)
	}

	t.Run("equal issued_at still installs and rebinds the proof", func(t *testing.T) {
		at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
		initial := mobileCatalogN(t, at, 2)
		store := &CatalogStore{}
		if err := store.Apply(initial.WithMobileProof(mobileProofFor(initial, 0, at)), "", "inst-1", at); err != nil {
			t.Fatal(err)
		}
		rebind := mobileCatalogN(t, at, 2)
		if err := store.Apply(rebind.WithMobileProof(mobileProofFor(rebind, 1, at)), "", "inst-1", at); err != nil {
			t.Fatalf("equal-IssuedAt proof rebind must still install: %v", err)
		}
		stored := store.Snapshot()
		if !stored.MobileAuthority() {
			t.Fatal("mobile provenance lost on equal-IssuedAt install")
		}
		if err := stored.ValidateNode("", "inst-1", "gw-1", at); err != nil {
			t.Fatalf("re-bound proof must admit gw-1: %v", err)
		}
		if err := stored.ValidateNode("", "inst-1", "gw-0", at); err == nil {
			t.Fatal("old selection must not keep admission")
		}
	})
}

func TestStoreSameRevisionRetainsFreshnessAndRebindsMobileProof(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	base := mobileCatalogN(t, now, 3)
	store := &CatalogStore{}
	if err := store.Apply(base.WithMobileProof(mobileProofFor(base, 0, now)), "", "inst-1", now); err != nil {
		t.Fatal(err)
	}

	refreshed := mobileCatalogN(t, now, 3)
	refreshed.CatalogExpiresAt = now.Add(12 * time.Minute).UTC().Format(time.RFC3339)
	for index := range refreshed.Nodes {
		refreshed.Nodes[index].Access.ExpiresAt = now.Add(12 * time.Minute).UTC().Format(time.RFC3339)
	}
	proof := mobileProofFor(refreshed, 2, now)
	proof.NotAfter = now.Add(12 * time.Minute)
	proof.CatalogValidUntil = now.Add(12 * time.Minute)
	if err := store.Apply(refreshed.WithMobileProof(proof), "", "inst-1", now); err != nil {
		t.Fatalf("same-revision freshness plus proof re-bind must be accepted: %v", err)
	}
	stored := store.Snapshot()
	if stored.CatalogExpiresAt != refreshed.CatalogExpiresAt ||
		stored.Nodes[2].Access.ExpiresAt != refreshed.Nodes[2].Access.ExpiresAt {
		t.Fatal("refreshed horizons were not retained")
	}
	if !stored.MobileAuthority() {
		t.Fatal("mobile provenance lost on same-revision refresh")
	}
	if err := stored.ValidateNode("", "inst-1", "gw-2", now); err != nil {
		t.Fatalf("re-bound proof must admit the new selection: %v", err)
	}
	if err := stored.ValidateNode("", "inst-1", "gw-0", now); err == nil {
		t.Fatal("the old selection must not keep admission")
	}
	// A material node change still fails the same-revision identity fence.
	material := mobileCatalogN(t, now, 3)
	material.Nodes[2].Access.Password = "other"
	if err := store.Apply(material.WithMobileProof(mobileProofFor(material, 2, now)), "", "inst-1", now); err == nil {
		t.Fatal("material same-revision change must stay REVISION_CONFLICT")
	}
}

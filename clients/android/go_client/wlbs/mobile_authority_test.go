package wlbs

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Mobile authority: only the typed proof may relax the legacy business envelope. The
// shared technical node/grant/endpoint/decimal/time checks stay in force, provenance
// survives cloning and is never reconstructed from JSON.

func mobileCatalog(t *testing.T) Catalog {
	t.Helper()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	catalog := Catalog{
		V: 2, Status: "ok",
		SubscriptionRef: "mobile:inst-1", RegistrationID: "inst-1",
		Revision: "7", IssuedAt: now.Format("2006-01-02T15:04:05Z"),
		RefreshAfter: now.Format("2006-01-02T15:04:05Z"), CatalogExpiresAt: now.Add(9 * time.Minute).Format("2006-01-02T15:04:05Z"),
		Nodes: []Node{{
			Endpoint: Endpoint{NodeID: "gw-1", PeerIP: "127.0.0.1", DTLSPort: 56000,
				DTLSSPKISHA256: EncodeBinary(make([]byte, 32))},
			Name: "Synthetic", CountryCode: "XX", WGPort: 56002, Protocol: "wdtt-v17",
			AuthMode: "installation-pop-v1", MaxWorkers: 36,
			Access: Access{GrantID: "g-1", DeviceID: "inst-1", Password: "synthetic",
				Generation: "1", LeaseSeq: "1", ExpiresAt: now.Add(9 * time.Minute).Format("2006-01-02T15:04:05Z")},
		}},
	}
	return catalog
}

func mobileProof(catalog Catalog, now time.Time) MobileCatalogProof {
	return MobileCatalogProof{
		NodeID: catalog.Nodes[0].NodeID, Subject: catalog.RegistrationID,
		CatalogRevision: catalog.Revision, GrantID: catalog.Nodes[0].Access.GrantID,
		Generation: catalog.Nodes[0].Access.Generation, LeaseSeq: catalog.Nodes[0].Access.LeaseSeq,
		NotAfter: now.Add(9 * time.Minute), CatalogValidUntil: now.Add(9 * time.Minute),
		EntitlementDeadline: now.Add(24 * time.Hour),
		TargetWorkers:       36, VerifiedAt: now,
	}
}

func TestMobileAuthorityAcceptsSharedChecksWithoutLegacyEnvelope(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	catalog := mobileCatalog(t)
	if err := catalog.Validate("mobile:inst-1", "inst-1", now); err == nil {
		t.Fatal("legacy strict validation must reject a catalog without vk_hashes")
	}
	trusted := catalog.WithMobileProof(mobileProof(catalog, now))
	if err := trusted.Validate("", "", now); err != nil {
		t.Fatalf("mobile authority must accept the verified snapshot: %v", err)
	}
	if err := trusted.ValidateNode("", "inst-1", "gw-1", now); err != nil {
		t.Fatalf("selected node must validate: %v", err)
	}
	if err := trusted.ValidateNode("", "inst-1", "other", now); err == nil {
		t.Fatal("a different node must not inherit the mobile proof")
	}
}

func TestMobileAuthoritySurvivesCloneAndStoreButNotJSON(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	catalog := mobileCatalog(t)
	trusted := catalog.WithMobileProof(mobileProof(catalog, now))
	store := &CatalogStore{}
	if err := store.Apply(trusted, "", "inst-1", now); err != nil {
		t.Fatalf("store apply: %v", err)
	}
	snapshot := store.Snapshot()
	if !snapshot.MobileAuthority() {
		t.Fatal("clone must preserve the typed mobile provenance")
	}
	if err := snapshot.ValidateNode("", "inst-1", "gw-1", now); err != nil {
		t.Fatalf("cloned snapshot must keep mobile admission: %v", err)
	}

	// Disk/network restore: JSON cannot resurrect trust, so the same bytes validate
	// only through a fresh coordinator proof.
	raw, err := json.Marshal(trusted)
	if err != nil {
		t.Fatal(err)
	}
	var restored Catalog
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.MobileAuthority() {
		t.Fatal("JSON restore must never reconstruct mobile provenance")
	}
	if err := restored.Validate("", "inst-1", now); err == nil {
		t.Fatal("restored untrusted catalog must fail strict validation")
	}
}

// The indefinite commercial right has no entitlement deadline of its own: the mobile
// projection substitutes the verified catalog validity as the finite proof deadline.
// That shape must validate, while a zero (unbounded) proof deadline must never.
func TestMobileAuthorityAcceptsCatalogBoundForIndefiniteRight(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	catalog := mobileCatalog(t)
	proof := mobileProof(catalog, now)
	proof.EntitlementDeadline = proof.CatalogValidUntil
	if err := catalog.WithMobileProof(proof).Validate("", "inst-1", now); err != nil {
		t.Fatalf("catalog-bounded indefinite proof must validate: %v", err)
	}
	proof.EntitlementDeadline = time.Time{}
	if err := catalog.WithMobileProof(proof).Validate("", "inst-1", now); err == nil {
		t.Fatal("a zero proof deadline must never be admitted")
	}
}

func TestMobileAuthorityRejectsMismatchedProofFields(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		mutate func(*Catalog, *MobileCatalogProof)
	}{
		{"lease seq", func(c *Catalog, p *MobileCatalogProof) { p.LeaseSeq = "2" }},
		{"generation", func(c *Catalog, p *MobileCatalogProof) { p.Generation = "2" }},
		{"grant", func(c *Catalog, p *MobileCatalogProof) { p.GrantID = "other" }},
		{"node", func(c *Catalog, p *MobileCatalogProof) { p.NodeID = "other" }},
		{"subject", func(c *Catalog, p *MobileCatalogProof) { p.Subject = "other" }},
		{"revision", func(c *Catalog, p *MobileCatalogProof) { p.CatalogRevision = "9" }},
		{"workers", func(c *Catalog, p *MobileCatalogProof) { p.TargetWorkers = 18 }},
		{"not after beyond deadline", func(c *Catalog, p *MobileCatalogProof) { p.NotAfter = p.EntitlementDeadline.Add(time.Minute) }},
		{"expired grant", func(c *Catalog, p *MobileCatalogProof) {
			c.Nodes[0].Access.ExpiresAt = now.Add(-time.Minute).Format("2006-01-02T15:04:05Z")
			p.NotAfter = now.Add(-time.Minute)
		}},
		{"unlimited grant", func(c *Catalog, p *MobileCatalogProof) { c.Nodes[0].Access.Unlimited = true }},
		{"catalog valid until mismatch", func(c *Catalog, p *MobileCatalogProof) { p.CatalogValidUntil = p.CatalogValidUntil.Add(time.Minute) }},
		{"catalog valid until expired", func(c *Catalog, p *MobileCatalogProof) {
			c.CatalogExpiresAt = now.Add(-time.Minute).Format("2006-01-02T15:04:05Z")
			p.CatalogValidUntil = now.Add(-time.Minute)
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			catalog := mobileCatalog(t)
			proof := mobileProof(catalog, now)
			testCase.mutate(&catalog, &proof)
			if err := catalog.WithMobileProof(proof).Validate("", "inst-1", now); err == nil {
				t.Fatalf("mismatched proof must not be admitted")
			}
		})
	}
}

func mobileCatalogN(t *testing.T, now time.Time, count int) Catalog {
	t.Helper()
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	catalog := Catalog{
		V: 2, Status: "ok",
		SubscriptionRef: "mobile:inst-1", RegistrationID: "inst-1",
		Revision: "7", IssuedAt: stamp(0), RefreshAfter: stamp(0),
		CatalogExpiresAt: stamp(9 * time.Minute),
	}
	for index := 0; index < count; index++ {
		spki := make([]byte, 32)
		spki[0] = byte(index)
		spki[1] = byte(index >> 8)
		catalog.Nodes = append(catalog.Nodes, Node{
			Endpoint: Endpoint{NodeID: fmt.Sprintf("gw-%d", index), PeerIP: "127.0.0.1", DTLSPort: 56000 + index,
				DTLSSPKISHA256: EncodeBinary(spki)},
			Name: fmt.Sprintf("Synthetic %d", index), CountryCode: "XX", WGPort: 57000 + index,
			Protocol: "wdtt-v17", AuthMode: "installation-pop-v1", MaxWorkers: 36,
			Access: Access{GrantID: fmt.Sprintf("g-%d", index), DeviceID: "inst-1", Password: "synthetic",
				Generation: "1", LeaseSeq: "1", ExpiresAt: stamp(9 * time.Minute)},
		})
	}
	return catalog
}

func mobileProofFor(catalog Catalog, nodeIndex int, now time.Time) MobileCatalogProof {
	node := catalog.Nodes[nodeIndex]
	return MobileCatalogProof{
		NodeID: node.NodeID, Subject: catalog.RegistrationID, CatalogRevision: catalog.Revision,
		GrantID: node.Access.GrantID, Generation: node.Access.Generation, LeaseSeq: node.Access.LeaseSeq,
		NotAfter: now.Add(9 * time.Minute), CatalogValidUntil: now.Add(9 * time.Minute),
		EntitlementDeadline: now.Add(24 * time.Hour), TargetWorkers: node.MaxWorkers, VerifiedAt: now,
	}
}

func TestMobileAuthoritySupportsArbitraryNodeCounts(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for _, count := range []int{3, 1000} {
		catalog := mobileCatalogN(t, now, count)
		trusted := catalog.WithMobileProof(mobileProofFor(catalog, count-1, now))
		if err := trusted.Validate("", "inst-1", now); err != nil {
			t.Fatalf("%d nodes: %v", count, err)
		}
		store := &CatalogStore{}
		if err := store.Apply(trusted, "", "inst-1", now); err != nil {
			t.Fatalf("%d nodes store: %v", count, err)
		}
		snapshot := store.Snapshot()
		if len(snapshot.Nodes) != count {
			t.Fatalf("catalog truncated: %d of %d", len(snapshot.Nodes), count)
		}
		if err := snapshot.ValidateNode("", "inst-1", catalog.Nodes[count-1].NodeID, now); err != nil {
			t.Fatalf("%d nodes selected: %v", count, err)
		}
		if err := snapshot.ValidateNode("", "inst-1", catalog.Nodes[0].NodeID, now); err == nil {
			t.Fatalf("%d nodes: a foreign node must not inherit the proof", count)
		}
	}
}

func TestMobileAuthorityRebindsProofAtSameRevisionWithoutExtendingContent(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	catalog := mobileCatalogN(t, now, 3)
	store := &CatalogStore{}
	if err := store.Apply(catalog.WithMobileProof(mobileProofFor(catalog, 0, now)), "", "inst-1", now); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(catalog.WithMobileProof(mobileProofFor(catalog, 2, now)), "", "inst-1", now); err != nil {
		t.Fatalf("same revision proof re-bind must be accepted: %v", err)
	}
	if err := store.Snapshot().ValidateNode("", "inst-1", "gw-2", now); err != nil {
		t.Fatalf("re-bound proof must admit the new selection: %v", err)
	}
	if err := store.Snapshot().ValidateNode("", "inst-1", "gw-0", now); err == nil {
		t.Fatal("the old selection must not keep admission")
	}
	changed := mobileCatalogN(t, now, 3)
	changed.Nodes[2].Access.Password = "other"
	if err := store.Apply(changed.WithMobileProof(mobileProofFor(changed, 2, now)), "", "inst-1", now); err == nil {
		t.Fatal("same revision business change must stay REVISION_CONFLICT")
	}
}

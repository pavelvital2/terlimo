package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// The expected catalog is identical for both encodings, before applying any
// proposed server patch. Production decoder/controller remain unchanged.
func TestCatalogEnvelopeReview09(t *testing.T) {
	old := runnerCatalog(time.Now().UTC().Truncate(time.Second))
	flat, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	nested := recoveryRaw(old, "ok")
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"OLD_flat", flat},
		{"NEW_nested", nested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := managedCatalogResponse(tc.raw)
			if err != nil || !reflect.DeepEqual(decoded, old) {
				t.Fatal("decoder did not yield unchanged full catalog", err)
			}
			c, base, _ := recoveryController(t, old)
			client := &fullCatalogParityClient{recoveryClient: base, raw: tc.raw}
			if err := c.synchronizeClient(context.Background(), client, false, 3); err != nil {
				t.Fatal("actual controller rejected catalog", err)
			}
			if !reflect.DeepEqual(c.store.Snapshot(), old) || !reflect.DeepEqual(c.saved.Catalog, old) || c.saved.Pending != nil || c.saved.Installation != "unchanged-key-fingerprint" || client.calls != 1 || len(base.trace) != 0 {
				t.Fatal("store/persistence/query/identity outcome differs")
			}
			t.Log("decoder=ACCEPT controller=ACCEPT live_store=expected_full_catalog pending=nil extra_mutations=0")
		})
	}
}

func TestFlatV2CatalogUsesCatalogVersionRatherThanOperationEnvelopeVersion(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cat := runnerCatalog(now)
	cat.V = 2
	cat.SubscriptionUnlimited = true
	cat.SubscriptionExpiresAt = ""
	for i := range cat.Nodes {
		cat.Nodes[i].Access.Unlimited = true
		cat.Nodes[i].Access.ExpiresAt = ""
	}
	raw, err := json.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := managedCatalogResponse(raw)
	if err != nil || !reflect.DeepEqual(decoded, cat) {
		t.Fatal("flat signed v2 catalog rejected", err)
	}
	c, base, _ := recoveryController(t, cat)
	client := &fullCatalogParityClient{recoveryClient: base, raw: raw}
	if err := c.synchronizeClient(context.Background(), client, false, 3); err != nil {
		t.Fatal("actual controller rejected flat signed v2 catalog", err)
	}
	if got := c.store.Snapshot(); !reflect.DeepEqual(got, cat) || c.saved.Pending != nil || client.calls != 1 {
		t.Fatal("flat v2 catalog was not committed exactly once")
	}
}

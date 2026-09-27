package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"wg-turn-client/wlbs"
)

// Exercises the application's actual response dispatcher, not a parallel test
// parser. Catalog validation uses the immutable fixture clock.
func TestManagedSignedSchemaResponses(t *testing.T) {
	raw, err := os.ReadFile("../docs/fixtures/wl_schema_signed_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != "edacbc6b0653a4bf63727fd9d8347405ece083b5bb578eaa9ab0f211a870c45d" {
		t.Fatal("fixture changed")
	}
	var fixture map[string]json.RawMessage
	if err := wlbs.StrictJSON(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	now, err := wlbs.UTC("2026-09-10T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"catalog", "register_success", "refresh_access_success", "operation_status_complete"} {
		t.Run(name, func(t *testing.T) {
			cat, err := managedCatalogResponse(fixture[name])
			if err != nil {
				t.Fatal(err)
			}
			store := &wlbs.CatalogStore{}
			if err := store.Apply(cat, "fixture-only", "22222222-2222-4222-8222-222222222222", now); err != nil {
				t.Fatal(err)
			}
			if store.Snapshot().Revision != "7" {
				t.Fatal("wrong snapshot")
			}
		})
	}
	for _, tc := range []struct{ name, code string }{
		{"operation_status_pending", "OPERATION_PENDING"},
		{"operation_status_unknown", "OPERATION_UNKNOWN"},
		{"operation_status_failed", "DEVICE_LIMIT_REACHED"},
		// Controller explicitly requests full snapshots (known_revision omitted).
		// Unsolicited not_modified must not create/renew a snapshot.
		{"catalog_not_modified", "BAD_MESSAGE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat, err := managedCatalogResponse(fixture[tc.name])
			if cat != nil || managedCode(err) != tc.code {
				t.Fatalf("code=%v wanted=%s", err, tc.code)
			}
		})
	}
	var negatives []struct {
		Field string          `json:"field"`
		Value json.RawMessage `json:"value"`
	}
	if err := wlbs.StrictJSON(fixture["negative_schema"], &negatives); err != nil {
		t.Fatal(err)
	}
	for _, n := range negatives {
		if n.Field == "operation_status" {
			cat, err := managedCatalogResponse(n.Value)
			if cat != nil || managedCode(err) != "BAD_CATALOG" {
				t.Fatal("incomplete status accepted", err)
			}
		}
	}
}

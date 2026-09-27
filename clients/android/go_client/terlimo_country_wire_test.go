package main

import (
	"encoding/json"
	"testing"
	"time"
)

func managedCatalogCountryWire(t *testing.T, value any, remove bool) []byte {
	t.Helper()
	catalog := runnerTwoNodes(time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC))
	raw, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal("FIXTURE_ENCODE")
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil {
		t.Fatal("FIXTURE_DECODE")
	}
	node := document["nodes"].([]any)[1].(map[string]any)
	if remove {
		delete(node, "country_code")
	} else {
		node["country_code"] = value
	}
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal("FIXTURE_ENCODE")
	}
	return raw
}

func TestManagedCountryCodeWireGuardResponseAndHistoricalReceipt(t *testing.T) {
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		value  any
		remove bool
		valid  bool
	}{
		{name: "empty", value: "", valid: true},
		{name: "two_char", value: "ZZ", valid: true},
		{name: "one_char", value: "Z"},
		{name: "three_char", value: "ZZZ"},
		{name: "missing", remove: true},
		{name: "null", value: nil},
		{name: "number", value: 7},
		{name: "boolean", value: false},
		{name: "array", value: []any{}},
		{name: "object", value: map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/managed_response", func(t *testing.T) {
			catalogRaw := managedCatalogCountryWire(t, tc.value, tc.remove)
			wrapped, err := json.Marshal(map[string]any{
				"v": 1, "status": "complete", "catalog": json.RawMessage(catalogRaw),
			})
			if err != nil {
				t.Fatal("FIXTURE_ENCODE")
			}
			catalog, err := managedCatalogResponse(wrapped)
			if err == nil {
				err = catalog.ValidateRefreshMetadata("sub", "reg", now)
			}
			if tc.valid {
				if err != nil {
					t.Fatal("VALID_COUNTRY_REJECTED", err)
				}
			} else if managedCode(err) != "BAD_CATALOG" {
				t.Fatalf("got %v want BAD_CATALOG", err)
			}
		})

		t.Run(tc.name+"/saved_historical_receipt", func(t *testing.T) {
			catalogRaw := managedCatalogCountryWire(t, tc.value, tc.remove)
			savedRaw, err := json.Marshal(map[string]any{
				"subscription_ref": "sub",
				"installation_id":  "installation",
				"completed_operation": map[string]any{
					"request_id": "immutable",
					"catalog":    json.RawMessage(catalogRaw),
				},
			})
			if err != nil {
				t.Fatal("FIXTURE_ENCODE")
			}
			var saved managedSaved
			err = decodeManagedSaved(savedRaw, &saved)
			if err == nil && saved.Completed != nil && saved.Completed.Catalog != nil {
				err = saved.Completed.Catalog.ValidateRefreshMetadata("sub", "reg", now)
			}
			if tc.valid {
				if err != nil || saved.Completed == nil || saved.Completed.Catalog == nil {
					t.Fatal("VALID_RECEIPT_REJECTED", err)
				}
			} else if managedCode(err) != "BAD_CATALOG" {
				t.Fatalf("got %v want BAD_CATALOG", err)
			}
		})
	}
}

func TestManagedCountryCodeKeepsUnrelatedEnvelopeFailure(t *testing.T) {
	_, err := managedCatalogResponse([]byte(`{"v":"wrong","status":"complete"}`))
	if managedCode(err) != "BAD_MESSAGE" {
		t.Fatalf("got %v want BAD_MESSAGE", err)
	}
	var saved managedSaved
	if err = decodeManagedSaved([]byte(`{"subscription_ref":7}`), &saved); managedCode(err) != "SAVED_STATE_INVALID" {
		t.Fatalf("got %v want SAVED_STATE_INVALID", err)
	}
}

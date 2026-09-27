package wlbs

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCountryCodeWirePresenceAndTypeAcrossCatalogPaths(t *testing.T) {
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	type historicalReceipt struct {
		RequestID string          `json:"request_id"`
		Catalog   json.RawMessage `json:"catalog"`
	}
	paths := map[string]func([]byte) error{
		"full": func(raw []byte) error {
			var catalog Catalog
			if err := StrictJSON(raw, &catalog); err != nil {
				return err
			}
			return catalog.Validate("fixture-only", "registration", now)
		},
		"recovery": func(raw []byte) error {
			var catalog Catalog
			if err := StrictJSON(raw, &catalog); err != nil {
				return err
			}
			return catalog.ValidateRefreshMetadata("fixture-only", "registration", now)
		},
		"complete": func(raw []byte) error {
			wrapped, err := json.Marshal(map[string]any{"v": 1, "status": "complete", "catalog": json.RawMessage(raw)})
			if err != nil {
				return err
			}
			var status OperationStatus
			if err = StrictJSON(wrapped, &status); err != nil {
				return err
			}
			if err = status.Validate(); err != nil {
				return err
			}
			return status.Catalog.ValidateRefreshMetadata("fixture-only", "registration", now)
		},
		"historical_receipt": func(raw []byte) error {
			wrapped, err := json.Marshal(map[string]any{"request_id": "immutable", "catalog": json.RawMessage(raw)})
			if err != nil {
				return err
			}
			var receipt historicalReceipt
			if err = StrictJSON(wrapped, &receipt); err != nil {
				return err
			}
			var catalog Catalog
			if err = StrictJSON(receipt.Catalog, &catalog); err != nil {
				return err
			}
			return catalog.ValidateRefreshMetadata("fixture-only", "registration", now)
		},
	}
	cases := []struct {
		name   string
		value  any
		remove bool
		valid  bool
	}{
		{name: "empty", value: "", valid: true},
		{name: "two_char", value: "ZZ", valid: true},
		{name: "missing", remove: true},
		{name: "null", value: nil},
		{name: "number", value: 7},
		{name: "boolean", value: false},
		{name: "array", value: []any{}},
		{name: "object", value: map[string]any{}},
	}
	for _, tc := range cases {
		for path, decode := range paths {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				catalog := twoNodeCatalog()
				setCatalogTimes(&catalog, now)
				raw, err := json.Marshal(catalog)
				if err != nil {
					t.Fatal("FIXTURE_ENCODE")
				}
				var document map[string]any
				if json.Unmarshal(raw, &document) != nil {
					t.Fatal("FIXTURE_DECODE")
				}
				node := document["nodes"].([]any)[1].(map[string]any)
				if tc.remove {
					delete(node, "country_code")
				} else {
					node["country_code"] = tc.value
				}
				raw, err = json.Marshal(document)
				if err != nil {
					t.Fatal("FIXTURE_ENCODE")
				}
				err = decode(raw)
				if tc.valid {
					if err != nil {
						t.Fatal("VALID_COUNTRY_REJECTED", err)
					}
				} else {
					wireCode(t, err, "BAD_CATALOG")
				}
			})
		}
	}
}

func TestCountryCodeStrictJSONDoesNotRemapUnrelatedDecodeErrors(t *testing.T) {
	var catalog Catalog
	wireCode(t, StrictJSON([]byte(`{"v":"wrong"}`), &catalog), "BAD_MESSAGE")
}

func TestCountryUnknownCatalogGates(t *testing.T) {
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	validators := map[string]func(*Catalog) error{
		"full":        func(c *Catalog) error { return c.Validate("fixture-only", "registration", now) },
		"recovery":    func(c *Catalog) error { return c.ValidateRefreshMetadata("fixture-only", "registration", now) },
		"admission_A": func(c *Catalog) error { return c.ValidateNode("fixture-only", "registration", "test-1", now) },
		"admission_B": func(c *Catalog) error { return c.ValidateNode("fixture-only", "registration", "test-2", now) },
	}
	cases := []struct {
		name   string
		mutate func(*Catalog)
		accept bool
	}{
		{"empty_B", func(c *Catalog) { c.Nodes[1].CountryCode = "" }, true},
		{"two_bytes_unchanged", func(c *Catalog) { c.Nodes[1].CountryCode = "ZZ" }, true},
		{"one_byte", func(c *Catalog) { c.Nodes[1].CountryCode = "Z" }, false},
		{"three_bytes", func(c *Catalog) { c.Nodes[1].CountryCode = "ZZZ" }, false},
		{"empty_bad_pin", func(c *Catalog) { c.Nodes[1].CountryCode = ""; c.Nodes[1].DTLSSPKISHA256 = "bad" }, false},
		{"empty_duplicate_grant", func(c *Catalog) { c.Nodes[1].CountryCode = ""; c.Nodes[1].Access.GrantID = c.Nodes[0].Access.GrantID }, false},
		{"empty_wrong_grant_binding", func(c *Catalog) { c.Nodes[1].CountryCode = ""; c.Nodes[1].Access.DeviceID = "other" }, false},
	}
	for _, tc := range cases {
		for gate, validate := range validators {
			t.Run(tc.name+"/"+gate, func(t *testing.T) {
				catalog := twoNodeCatalog()
				setCatalogTimes(&catalog, now)
				catalog.Nodes[1].Name = "TEST2"
				tc.mutate(&catalog)
				err := validate(&catalog)
				if tc.accept {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					wireCode(t, err, "BAD_CATALOG")
				}
			})
		}
	}
}

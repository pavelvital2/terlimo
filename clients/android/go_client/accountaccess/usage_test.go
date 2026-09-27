package accountaccess

import (
	"strings"
	"testing"
)

const usageValid = `{
  "as_of": "2026-09-26T09:32:13Z",
  "buckets": [
    {"complete": false, "period": "today", "rx_bytes": 145776, "tx_bytes": 342200},
    {"complete": false, "period": "7d", "rx_bytes": 145776, "tx_bytes": 342200},
    {"complete": false, "period": "30d", "rx_bytes": 145776, "tx_bytes": 342200}
  ],
  "coverage_start": "2026-09-26T09:27:13Z",
  "request_id": "0123456789abcdef0123456789abcdef",
  "schema_version": "1.0",
  "server_time": "2026-09-26T09:32:14Z",
  "status": "ok",
  "timezone": "Europe/Moscow"
}`

func TestDecodeUsageStrictValid(t *testing.T) {
	usage, err := DecodeUsageStrict([]byte(usageValid))
	if err != nil {
		t.Fatalf("valid usage rejected: %v", err)
	}
	if usage.AsOf == nil || *usage.AsOf != "2026-09-26T09:32:13Z" || usage.Timezone != "Europe/Moscow" {
		t.Fatalf("usage fields wrong: %+v", usage)
	}
	if usage.CoverageStart == nil || *usage.CoverageStart != "2026-09-26T09:27:13Z" {
		t.Fatalf("coverage_start wrong: %+v", usage.CoverageStart)
	}
	if len(usage.Buckets) != 3 || usage.Buckets[0].RXBytes != 145776 ||
		usage.Buckets[0].TXBytes != 342200 || usage.Buckets[0].Complete {
		t.Fatalf("bucket wrong: %+v", usage.Buckets[0])
	}
}

func TestDecodeUsageStrictAcceptsNullCoverage(t *testing.T) {
	raw := strings.Replace(usageValid, `"coverage_start": "2026-09-26T09:27:13Z"`, `"coverage_start": null`, 1)
	usage, err := DecodeUsageStrict([]byte(raw))
	if err != nil {
		t.Fatalf("null coverage_start rejected: %v", err)
	}
	if usage.CoverageStart != nil {
		t.Fatalf("coverage_start should be null, got %v", *usage.CoverageStart)
	}
}

func TestDecodeUsageStrictAcceptsNullAsOfAndEmptyHistory(t *testing.T) {
	raw := strings.NewReplacer(
		`"as_of": "2026-09-26T09:32:13Z"`, `"as_of": null`,
		`"coverage_start": "2026-09-26T09:27:13Z"`, `"coverage_start": null`,
		`"rx_bytes": 145776, "tx_bytes": 342200`, `"rx_bytes": 0, "tx_bytes": 0`,
	).Replace(usageValid)
	usage, err := DecodeUsageStrict([]byte(raw))
	if err != nil {
		t.Fatalf("null as_of / empty history rejected: %v", err)
	}
	if usage.AsOf != nil || usage.CoverageStart != nil {
		t.Fatalf("times should be null: %v %v", usage.AsOf, usage.CoverageStart)
	}
	if len(usage.Buckets) != 3 || usage.Buckets[0].RXBytes != 0 || usage.Buckets[0].TXBytes != 0 {
		t.Fatalf("empty-history buckets wrong: %+v", usage.Buckets)
	}
}

func TestDecodeUsageStrictRejectsBadPayloads(t *testing.T) {
	cases := map[string]string{
		"unknown field":  strings.Replace(usageValid, `"status": "ok",`, `"status": "ok", "extra": 1,`, 1),
		"negative rx":    strings.Replace(usageValid, `"rx_bytes": 145776`, `"rx_bytes": -1`, 1),
		"bad period":     strings.Replace(usageValid, `"period": "today"`, `"period": "1h"`, 1),
		"duplicate":      strings.Replace(usageValid, `"period": "7d"`, `"period": "today"`, 1),
		"bad timezone":   strings.Replace(usageValid, `"timezone": "Europe/Moscow"`, `"timezone": "UTC"`, 1),
		"bad as_of":      strings.Replace(usageValid, `"as_of": "2026-09-26T09:32:13Z"`, `"as_of": "yesterday"`, 1),
		"missing bucket": `{"request_id":"0123456789abcdef0123456789abcdef","schema_version":"1.0","server_time":"2026-09-26T09:32:14Z","status":"ok","timezone":"Europe/Moscow","as_of":"2026-09-26T09:32:13Z","coverage_start":null,"buckets":[]}`,
	}
	for name, raw := range cases {
		if _, err := DecodeUsageStrict([]byte(raw)); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}

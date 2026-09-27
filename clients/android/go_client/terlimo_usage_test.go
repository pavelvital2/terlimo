package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wg-turn-client/accountaccess"
)

func usageTestMobile(t *testing.T, server *httptest.Server) (*managedMobile, *strings.Builder) {
	t.Helper()
	if server != nil {
		t.Cleanup(server.Close)
	}
	client := &accountaccess.Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: accountaccess.StaticToken("test-bearer")}
	out := &strings.Builder{}
	bridge := newManagedBridge(out, "attempt-usage", func() {})
	return &managedMobile{client: client, bridge: bridge}, out
}

func lastBridgeLine(t *testing.T, out *strings.Builder) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) == 0 || lines[len(lines)-1] == "" {
		t.Fatal("no bridge event written")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatalf("bridge event not JSON: %v (%q)", err, lines[len(lines)-1])
	}
	return m
}

func TestHandleUsageReadEmitsStrictResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/usage" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-bearer" {
			t.Fatalf("bearer missing: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(usageFixtureBody))
	}))
	mobile, out := usageTestMobile(t, server)
	mobile.handleUsageRead(context.Background())
	event := lastBridgeLine(t, out)
	if event["type"] != "usage_result" || event["state"] != "ok" || event["attempt_id"] != "attempt-usage" {
		t.Fatalf("event envelope wrong: %v", event)
	}
	if event["as_of"] != "2026-09-26T09:32:13Z" || event["coverage_start"] != "2026-09-26T09:27:13Z" {
		t.Fatalf("time fields wrong: %v", event)
	}
	buckets, ok := event["buckets"].([]any)
	if !ok || len(buckets) != 3 {
		t.Fatalf("buckets wrong: %v", event["buckets"])
	}
	first := buckets[0].(map[string]any)
	if first["period"] != "today" || first["rx_bytes"].(float64) != 145776 ||
		first["tx_bytes"].(float64) != 342200 || first["complete"].(bool) {
		t.Fatalf("bucket wrong: %v", first)
	}
}

func TestHandleUsageReadMapsStatusAndMalformedToBoundedCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		code   string
	}{
		"401":       {401, `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-26T09:32:14Z","schema_version":"1.0","status":"error","code":"SESSION_EXPIRED","retryable":false,"retry_after_ms":null,"message_key":null,"details":{}}`, "SESSION_EXPIRED"},
		"403":       {403, `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-26T09:32:14Z","schema_version":"1.0","status":"error","code":"DEVICE_REVOKED","retryable":false,"retry_after_ms":null,"message_key":null,"details":{}}`, "DEVICE_REVOKED"},
		"malformed": {200, `{"schema_version":"1.0","status":"ok","unexpected":true}`, "TRANSPORT"},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := tc.status, tc.body
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}))
			mobile, out := usageTestMobile(t, server)
			mobile.handleUsageRead(context.Background())
			event := lastBridgeLine(t, out)
			if event["type"] != "usage_result" || event["state"] != "error" || event["code"] != tc.code {
				t.Fatalf("want error %s, got %v", tc.code, event)
			}
		})
	}
}

const usageFixtureBody = `{
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

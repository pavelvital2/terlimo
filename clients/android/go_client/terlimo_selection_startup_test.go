package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wg-turn-client/wlbs"
)

// Drive the real runMobile -> catalogOperations -> Runner -> Coordinator HTTP
// dispatch. In particular, the first cycle has the host's catalog_cycle tag.
func TestMobileSelectionStartupOrdering(t *testing.T) {
	for _, kind := range []string{"present", "browse_removed", "changed_subject", "credential_removed", "explicit_clear"} {
		t.Run(kind, func(t *testing.T) {
			f := newMobileFeedFixture(t, 2, mobileFeedOptions{})
			var removeBrowse atomic.Bool
			var browseCalls, unexpectedWrites atomic.Int32
			backend := f.handler()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet && req.URL.Path != "/api/mobile/v1/auth/challenge" && req.URL.Path != "/api/mobile/v1/auth/session" {
					unexpectedWrites.Add(1)
				}
				if req.URL.Path == "/api/mobile/v1/gateways" && req.URL.Query().Get("view") == "browse" {
					browseCalls.Add(1)
					nodes := []map[string]any{{"gateway_id": "gw-0", "name": "B"}}
					if !removeBrowse.Load() {
						nodes = append(nodes, map[string]any{"gateway_id": "gw-1", "name": "A"})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"request_id": "0123456789abcdef0123456789abcdef", "server_time": feedStamp(time.Now()),
						"schema_version": "1.0", "status": "ok", "catalog_mode": "browse",
						"issued_at": feedStamp(f.issuedAt), "valid_until": feedStamp(f.validUntil), "gateways": nodes,
					})
					return
				}
				backend.ServeHTTP(w, req)
			}))
			defer server.Close()
			waitCatalog := func(host *feedHost, predicate func(map[string]any) bool) map[string]any {
				t.Helper()
				until := time.Now().Add(5 * time.Second)
				for time.Now().Before(until) {
					if frame, ok := host.find("catalog"); ok && predicate(frame) {
						return frame
					}
					time.Sleep(5 * time.Millisecond)
				}
				t.Fatal("expected catalog did not reach real bridge")
				return nil
			}
			credential := func(frame map[string]any) bool { return frame["catalog_mode"] != "browse" }
			// Establish A via the real manual choose command and acknowledged catalog.
			old, _, oldHost, stopOld, oldDone := newMobileFeedRun(t, f, mobileFeedStart(t, f, server))
			waitCatalog(oldHost, credential)
			oldHost.send(t, map[string]any{"type": "choose_node", "node_id": "gw-1"})
			ack := waitCatalog(oldHost, func(frame map[string]any) bool { return frame["selected_node_id"] == "gw-1" })
			if old.selectionID() != "gw-1" {
				t.Fatal("choose not acknowledged")
			}
			stopOld()
			<-oldDone
			pref := &mobileSelectionPreference{NodeID: ack["selected_node_id"].(string), InstallationID: f.fingerprint, BaseURL: server.URL, Environment: "test"}
			if kind == "browse_removed" {
				removeBrowse.Store(true)
			}
			if kind == "changed_subject" {
				pref.AccountRef = "previous-account"
			}
			start := mobileFeedStart(t, f, server)
			start.CatalogCycle = "00000000-0000-4000-8000-000000000001"
			start.MobileSelection = pref
			raw, _ := json.Marshal(start)
			if err := wlbs.StrictJSON(raw, &start); err != nil {
				t.Fatal(err)
			}
			c, _, host, stop, done := newMobileFeedRun(t, f, start)
			var stopped sync.Once
			finish := func() { stopped.Do(func() { stop(); <-done }) }
			defer finish()
			browse := waitCatalog(host, func(frame map[string]any) bool { return frame["catalog_mode"] == "browse" })
			if browseCalls.Load() != 1 || browse["catalog_cycle"] != start.CatalogCycle {
				t.Fatal("startup did not dispatch display-only cycle")
			}
			if _, exists := browse["selected_node_id"]; exists {
				t.Fatal("browse published credential selection")
			}
			if c.store.Snapshot() != nil || c.selectionID() != "" {
				t.Fatal("browse preference became authority")
			}
			c.mu.Lock()
			pending := c.start.MobileSelection != nil
			c.mu.Unlock()
			wantPending := kind == "present" || kind == "credential_removed" || kind == "explicit_clear"
			if pending != wantPending {
				t.Fatalf("startup browse retained preference=%v, want=%v", pending, wantPending)
			}
			// The test wakes the existing ordinary cadence; the product adds no request.
			removeBrowse.Store(false)
			healthyNodes := f.gatewayBodies()
			if kind == "credential_removed" {
				f.setCatalog("8", healthyNodes[:1])
			}
			if kind == "explicit_clear" {
				c.clearSelection()
			}
			c.mobile.runner.Trigger("test-next-ordinary-cycle")
			frame := waitCatalog(host, credential)
			want := ""
			if kind == "present" {
				want = "gw-1"
			}
			if frame["selected_node_id"] != want || c.selectionID() != want {
				t.Fatalf("fresh pair selected=%v/native=%q want=%q", frame["selected_node_id"], c.selectionID(), want)
			}
			if want != "" && c.store.Snapshot().ValidateNode("", "", want, time.Now()) != nil {
				t.Fatal("restored without admitted current proof")
			}
			// An authoritative clear is final, even when a later accepted pair includes A.
			f.setCatalog("9", healthyNodes)
			c.mobile.runner.Trigger("test-healthy-cycle")
			waitCatalog(host, func(frame map[string]any) bool { return credential(frame) && frame["revision"] == "9" })
			finish()
			if c.selectionID() != want {
				t.Fatal("cleared preference revived on healthy pair")
			}
			// Model the host's acknowledged credential write (exact selected ID or clear),
			// serialize start again and repeat the real startup dispatch in a new attempt.
			start.MobileSelection = nil
			if want != "" {
				start.MobileSelection = pref
			}
			restarted, _, nextHost, stopNext, nextDone := newMobileFeedRun(t, f, start)
			defer func() { stopNext(); <-nextDone }()
			waitCatalog(nextHost, func(frame map[string]any) bool { return frame["catalog_mode"] == "browse" })
			restarted.mobile.runner.Trigger("test-restart-ordinary-cycle")
			next := waitCatalog(nextHost, credential)
			if next["selected_node_id"] != want || restarted.selectionID() != want {
				t.Fatal("restart revived/changed preference")
			}
			if unexpectedWrites.Load() != 0 {
				t.Fatal("selection restore added a mutating HTTP operation")
			}
			host.mu.Lock()
			defer host.mu.Unlock()
			for _, msg := range host.messages {
				if msg["type"] == "vpn_config" || msg["type"] == "node_probe_result" {
					t.Fatal("automatic Connect/probe")
				}
			}
		})
	}
}

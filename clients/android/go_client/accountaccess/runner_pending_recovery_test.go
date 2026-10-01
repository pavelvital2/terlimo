package accountaccess

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPendingPollRecoveryStartsInSameCycle(t *testing.T) {
	marker := fenceOperationBody("op-1", "retryable_failure", "", `{"required":true,"catalog_revision":"20","binding_revision":"1"}`)
	fixture, server := newRecoveryRunnerServer(t, "20", marker, syncBody("op-2", "pending"))
	_, _, oldKey := newRecoveryHarness(t, fixture, server, "op-1")
	fixture.store.Value.Response.AccessApplicationState = "pending"
	runner := newRecoveryRunnerFromStore(t, fixture, server)
	if _, _, err := runner.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := fixture.eventSnapshot()
	if len(events) != 2 || events[0] != "GET /api/mobile/v1/operations/op-1?observe=sync_recovery" || events[1] != "POST /api/mobile/v1/access/sync?observe=none" {
		t.Fatalf("pending poll must hand recovery to existing coordinator within same cycle, got %v", events)
	}
	key, _, persisted := fixture.postSnapshot()
	if key == oldKey || persisted == nil || persisted.Key != key || persisted.RotationOf != "op-1" {
		t.Fatalf("recovery must remain durable and rotate only old operation")
	}
}

// A marker-approved POST is accepted asynchronously. Its pending response must
// still require a verified catalog, including on the new same-cycle path.
func TestPendingPollRecoveryStillRequiresReadyCatalog(t *testing.T) {
	marker := fenceOperationBody("op-1", "retryable_failure", "", `{"required":true,"catalog_revision":"20","binding_revision":"1"}`)
	fixture, originalServer := newRecoveryRunnerServer(t, "20", marker, syncBody("op-2", "pending"))
	var gatewayCalls atomic.Int32
	delegate := fixture.handler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/mobile/v1/gateways" {
			delegate.ServeHTTP(w, req)
			return
		}
		gatewayCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"ACCESS_SYNC_PENDING","retryable":true,"details":{"catalog_revision":"20","binding_revision":"1"}}`)
	}))
	t.Cleanup(server.Close)
	_, _, oldKey := newRecoveryHarness(t, fixture, originalServer, "op-1")
	fixture.store.Value.Response.AccessApplicationState = "pending"
	runner := newRecoveryRunnerFromStore(t, fixture, server)
	verified := false
	runner.config.OnVerified = func(MeResponse, CatalogResponse) { verified = true }
	catalog, browse, err := runner.cycle(context.Background())
	if err == nil || err.Error() != "ACCESS_SYNC_PENDING" || catalog != nil || browse != nil || verified {
		t.Fatalf("pending POST/catalog cannot signal readiness: err=%v catalog=%v browse=%v verified=%v", err, catalog, browse, verified)
	}
	if gatewayCalls.Load() != 2 {
		t.Fatalf("catalog must be refreshed after recovery POST, got %d reads", gatewayCalls.Load())
	}
	key, _, durable := fixture.postSnapshot()
	if key == oldKey || durable == nil || durable.Key != key || fixture.store.Value.Response.OperationID != "op-2" || fixture.store.Value.Response.AccessApplicationState != "pending" {
		t.Fatal("new pending receipt must stay durable without declaring readiness")
	}
}

// Exercise the newly added pending-poll branch with a response lost AFTER the
// marker-approved key is persisted; restart must resend that exact new effect.
func TestPendingPollRecoveryLostResponseKeepsKeyOnRestart(t *testing.T) {
	marker := fenceOperationBody("op-1", "retryable_failure", "", `{"required":true,"catalog_revision":"20","binding_revision":"1"}`)
	fixture, server := newRecoveryRunnerServer(t, "20", marker, syncBody("op-2", "pending"))
	_, _, oldKey := newRecoveryHarness(t, fixture, server, "op-1")
	fixture.store.Value.Response.AccessApplicationState = "pending"
	fixture.cutSync = true
	runner := newRecoveryRunnerFromStore(t, fixture, server)
	if _, _, err := runner.cycle(context.Background()); err == nil {
		t.Fatal("lost recovery response must propagate an error")
	}
	pending := *fixture.store.Value
	key, body, durable := fixture.postSnapshot()
	if key == oldKey || key != pending.Key || pending.Response != nil || durable == nil || durable.Key != key || durable.Response != nil || pending.RotationOf != "op-1" {
		t.Fatal("lost response must retain the new key persisted before POST")
	}
	fixture.mu.Lock()
	fixture.cutSync = false
	fixture.operationBody = fenceOperationBody("op-2", "pending", "", `{"required":false}`)
	fixture.events = nil
	fixture.mu.Unlock()
	restarted := newRecoveryRunnerFromStore(t, fixture, server)
	if _, _, err := restarted.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	newKey, newBody, _ := fixture.postSnapshot()
	events := fixture.eventSnapshot()
	if newKey != key || newBody != body || len(events) != 2 || events[0] != "POST /api/mobile/v1/access/sync?observe=none" || events[1] != "GET /api/mobile/v1/operations/op-2?observe=sync_recovery" {
		t.Fatalf("restart must resend exact persisted effect without another rotation: events=%v", events)
	}
}

func TestPendingPollWithoutRecoveryNeverStartsNewEffect(t *testing.T) {
	for _, state := range []string{"pending", "applied", "rejected", "retryable_failure"} {
		t.Run(state, func(t *testing.T) {
			marker := fenceOperationBody("op-1", state, "", `{"required":false}`)
			fixture, server := newRecoveryRunnerServer(t, "20", marker, syncBody("op-2", "pending"))
			newRecoveryHarness(t, fixture, server, "op-1")
			fixture.store.Value.Response.AccessApplicationState = "pending"
			runner := newRecoveryRunnerFromStore(t, fixture, server)
			if _, _, err := runner.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			events := fixture.eventSnapshot()
			if len(events) != 1 || events[0] != "GET /api/mobile/v1/operations/op-1?observe=sync_recovery" {
				t.Fatalf("unmarked state must not POST: %v", events)
			}
		})
	}
}

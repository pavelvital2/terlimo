package accountaccess

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeServer struct {
	mu              sync.Mutex
	meCalls         int
	gatewayCalls    int
	syncCalls       int
	operationCalls  int
	meBody          string
	gatewayBody     string
	gatewayStatus   int
	gatewayCode     string
	gatewayTokens   string
	syncResponse    string
	syncStatus      int
	lastSyncKey     string
	lastSyncBody    string
	meDelay         time.Duration
	operationBody   string
	operationStatus int
	meBlock         chan struct{}
	gatewayBlock    chan struct{}
	operationBlock  chan struct{}
}

func (f *fakeServer) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/me":
			f.mu.Lock()
			f.meCalls++
			body, delay, block := f.meBody, f.meDelay, f.meBlock
			f.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			if block != nil {
				<-block
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, body)
		case request.Method == http.MethodGet && request.URL.Path == "/gateways":
			f.mu.Lock()
			f.gatewayCalls++
			status, body, block := f.gatewayStatus, f.gatewayBody, f.gatewayBlock
			if status == 0 {
				status = http.StatusOK
			}
			f.mu.Unlock()
			if block != nil {
				<-block
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(status)
			_, _ = io.WriteString(writer, body)
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/operations/"):
			f.mu.Lock()
			f.operationCalls++
			status, body, block := f.operationStatus, f.operationBody, f.operationBlock
			if status == 0 {
				status = http.StatusOK
			}
			f.mu.Unlock()
			if block != nil {
				<-block
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(status)
			_, _ = io.WriteString(writer, body)
		case request.Method == http.MethodPost && request.URL.Path == "/access/sync":
			f.mu.Lock()
			f.syncCalls++
			f.lastSyncKey = request.Header.Get("Idempotency-Key")
			raw, _ := io.ReadAll(request.Body)
			f.lastSyncBody = string(raw)
			status, body := f.syncStatus, f.syncResponse
			if status == 0 {
				status = http.StatusOK
			}
			f.mu.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(status)
			_, _ = io.WriteString(writer, body)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	})
}

func syncBody(operationID, state string) string {
	return fmt.Sprintf(`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z",
		"schema_version":"1.0","status":"ok","operation_id":%q,"access_application_state":%q,
		"revision":"5","grants":[{"grant_id":"grant-synth-1","binding_ref":"bind-synth-1",
		"gateway_id":"gw-synth-1","desired_generation":"1","applied_generation":"1",
		"not_after":"2026-09-21T12:10:00Z","state":"active"}]}`, operationID, state)
}

func newTestCoordinator(t *testing.T, fake *fakeServer, subject *Subject) (*Coordinator, *MemoryReceiptStore) {
	t.Helper()
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	store := &MemoryReceiptStore{}
	coordinator, err := NewCoordinator(Options{
		Client:    &Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: StaticToken("test-bearer")},
		AttemptID: "attempt",
		Store:     store,
		Subject:   func() Subject { return *subject },
	})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, store
}

func TestFirstRefreshEmitsLinkedProjectionAndNullBindingBlocksSync(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, store := newTestCoordinator(t, fake, &subject)
	fake.meBody = meBody("7", nil, "active")

	result, err := coordinator.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if result.State != StateApplied || result.Projection == nil {
		t.Fatalf("expected applied projection, got %+v", result)
	}
	payload := result.Projection.Payload()
	if payload["session_generation"] != "1" || payload["previous_session_generation"] != nil {
		t.Fatalf("first generation pair wrong: %v", payload)
	}
	if payload["access_revision"] != "7" {
		t.Fatalf("access revision wrong: %v", payload["access_revision"])
	}
	if _, err := coordinator.SyncAccess(context.Background()); err != ErrNoBinding {
		t.Fatalf("sync without binding: %v", err)
	}
	if fake.syncCalls != 0 || store.Value != nil {
		t.Fatal("null binding must not create a receipt or a request")
	}
	if fake.meCalls != 1 {
		t.Fatalf("me calls=%d", fake.meCalls)
	}
}

func TestDuplicateStaleAndConflictKeepLastGood(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	first, err := coordinator.Refresh(context.Background())
	if err != nil || first.Projection == nil {
		t.Fatalf("first refresh failed: %v", err)
	}

	duplicate, err := coordinator.Refresh(context.Background())
	if err != nil || !duplicate.Duplicate || duplicate.Projection != nil {
		t.Fatalf("same revision and digest must dedupe: %+v %v", duplicate, err)
	}

	fake.meBody = meBody("6", &binding, "active")
	stale, err := coordinator.Refresh(context.Background())
	if err != nil || stale.Rejection != "STALE" || stale.Projection != nil {
		t.Fatalf("lower revision must be stale: %+v %v", stale, err)
	}

	conflictBody := strings.Replace(meBody("7", &binding, "active"), `"slots_used":1`, `"slots_used":2`, 1)
	fake.meBody = conflictBody
	conflict, err := coordinator.Refresh(context.Background())
	if err != nil || conflict.State != StateConflict || conflict.Rejection != "REVISION_CONFLICT" || conflict.Projection != nil {
		t.Fatalf("same revision different digest must conflict: %+v %v", conflict, err)
	}
}

func TestSubjectReplacementOpensFreshBaseline(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("12", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	subject = Subject{AccountRef: "acc-2", InstallationID: "inst-1"}
	fake.meBody = meBody("11", &binding, "active")
	result, err := coordinator.Refresh(context.Background())
	if err != nil || result.Projection == nil {
		t.Fatalf("new subject must accept its first revision as baseline: %+v %v", result, err)
	}
}

func TestSessionReplacementLinksPredecessorAndKeepsDomainBaseline(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("12", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	coordinator.NewSession()
	fake.meBody = meBody("12", &binding, "active")
	replacement, err := coordinator.Refresh(context.Background())
	if err != nil || replacement.Projection == nil {
		t.Fatalf("same revision after session replacement must still emit: %+v %v", replacement, err)
	}
	payload := replacement.Projection.Payload()
	if payload["session_generation"] != "2" || payload["previous_session_generation"] != "1" {
		t.Fatalf("replacement pair wrong: %v %v", payload["session_generation"], payload["previous_session_generation"])
	}
	fake.meBody = meBody("11", &binding, "active")
	stale, err := coordinator.Refresh(context.Background())
	if err != nil || stale.Rejection != "STALE" {
		t.Fatalf("bearer refresh must keep the revision baseline: %+v %v", stale, err)
	}
}

func TestRefreshCoalescesToExactlyOneTrailingAttempt(t *testing.T) {
	fake := &fakeServer{meDelay: 80 * time.Millisecond}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	fake.meBody = meBody("7", nil, "active")

	firstResult := make(chan Result, 1)
	go func() {
		result, _ := coordinator.Refresh(context.Background())
		firstResult <- result
	}()
	time.Sleep(20 * time.Millisecond)
	coalesced, err := coordinator.Refresh(context.Background())
	if err != nil || !coalesced.Coalesced {
		t.Fatalf("expected coalesced trigger: %+v %v", coalesced, err)
	}
	<-firstResult
	deadline := time.Now().Add(2 * time.Second)
	for {
		fake.mu.Lock()
		calls := fake.meCalls
		fake.mu.Unlock()
		if calls == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("coalescing produced %d me calls, want exactly 2", calls)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.meCalls != 2 {
		t.Fatalf("me calls=%d, want exactly one trailing refresh", fake.meCalls)
	}
}

func TestRefreshFailureNeverEmitsAndKeepsLastGood(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("4", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"SERVICE_UNAVAILABLE","retryable":true,"details":{"reason":"access_application_failed"}}`
	fake.gatewayStatus = http.StatusServiceUnavailable
	catalogResult, err := coordinator.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if catalogResult.Catalog != nil && len(catalogResult.Catalog.Gateways) != 1 {
		t.Fatal("last-good catalog must be retained on 503")
	}
	if coordinator.LastGood() == nil || len(coordinator.LastGood().Gateways) != 1 {
		t.Fatal("last-good catalog lost after 503")
	}
	fake.gatewayStatus = 0
	fake.gatewayBody = catalogBody("5", 0)
	emptyResult, err := coordinator.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !emptyResult.Applied || !emptyResult.AuthoritativeEmpty || len(coordinator.LastGood().Gateways) != 0 {
		t.Fatalf("authoritative empty 200 must be adopted: %+v", emptyResult)
	}
}

func TestCatalogNonterminalPendingKeepsLastGoodAndTokens(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("4", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayStatus = http.StatusConflict
	fake.gatewayBody = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"ACCESS_SYNC_PENDING","retryable":true,"retry_after_ms":1500,"details":{"catalog_revision":"5","binding_revision":"1"}}`
	result, err := coordinator.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Admission == nil || result.Admission.CatalogRevision != "5" || result.Admission.BindingRevision != "1" {
		t.Fatalf("admission tokens missing: %+v", result.Admission)
	}
	if len(coordinator.LastGood().Gateways) != 1 {
		t.Fatal("409 pending must not clear last-good")
	}
	fake.syncResponse = syncBody("op-1", "applied")
	sync, err := coordinator.SyncAccess(context.Background())
	if err != nil || sync.Receipt == nil {
		t.Fatalf("sync: %+v %v", sync, err)
	}
	if !strings.Contains(fake.lastSyncBody, `"catalog_revision":"5"`) {
		t.Fatalf("sync must carry the admission catalog revision: %s", fake.lastSyncBody)
	}
}

func TestSyncIdempotencyReceiptIsEffectOnce(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, store := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("4", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.syncResponse = syncBody("op-1", "applied")
	first, err := coordinator.SyncAccess(context.Background())
	if err != nil || first.Receipt == nil {
		t.Fatalf("first sync: %+v %v", first, err)
	}
	if store.Value == nil || store.Value.Key == "" || len(store.Value.Key) < 16 {
		t.Fatalf("receipt must be persisted with a bounded key: %+v", store.Value)
	}
	key := fake.lastSyncKey
	fake.syncResponse = syncBody("op-2", "applied")
	second, err := coordinator.SyncAccess(context.Background())
	if err != nil || !second.Replayed {
		t.Fatalf("terminal receipt must replay without a new effect: %+v %v", second, err)
	}
	if fake.syncCalls != 1 || fake.lastSyncKey != key {
		t.Fatalf("same persisted key/body must never start a new attempt: calls=%d", fake.syncCalls)
	}
}

func TestSyncPendingPollsWithoutNewEffectAndRetryableReusesKey(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("4", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.syncResponse = syncBody("op-1", "pending")
	first, err := coordinator.SyncAccess(context.Background())
	if err != nil || first.Receipt == nil || first.Replayed {
		t.Fatalf("pending sync: %+v %v", first, err)
	}
	replay, err := coordinator.SyncAccess(context.Background())
	if err != nil || !replay.Replayed || fake.syncCalls != 1 {
		t.Fatalf("pending must not start a new effect: %+v calls=%d %v", replay, fake.syncCalls, err)
	}
}

func TestSyncRetryableFailureRetriesWithSamePersistedKey(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("4", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.syncResponse = syncBody("op-1", "retryable_failure")
	first, err := coordinator.SyncAccess(context.Background())
	if err != nil || first.Receipt == nil {
		t.Fatalf("retryable sync: %+v %v", first, err)
	}
	key := fake.lastSyncKey
	fake.syncResponse = syncBody("op-1", "applied")
	second, err := coordinator.SyncAccess(context.Background())
	if err != nil || second.Receipt == nil || second.Receipt.Response.AccessApplicationState != "applied" {
		t.Fatalf("retry with the same key must be allowed: %+v %v", second, err)
	}
	if fake.syncCalls != 2 || fake.lastSyncKey != key {
		t.Fatalf("retry must reuse the persisted key: calls=%d key=%q want %q", fake.syncCalls, fake.lastSyncKey, key)
	}
}

func strPtr(value string) *string { return &value }

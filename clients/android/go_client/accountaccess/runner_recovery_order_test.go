package accountaccess

// Integration gate for the recovery call order through the real Runner cycle: with a
// persisted old retryable_failure receipt (19/1) and the opt-in server marker, the read-only
// GET /operations/{id}?observe=sync_recovery must happen BEFORE any POST /access/sync, the
// new key/body must be durable before that POST, and a probe transition to pending or a
// terminal state must never emit a stale POST. This catches the bf183ace runner-order defect
// that the coordinator-only tests could not see.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recoveryRunnerServer struct {
	mu             sync.Mutex
	auth           *authFixture
	gatewayCalls   int
	gatewayTokens  string
	operationBody  string
	syncResponse   string
	cutSync        bool
	events         []string
	store          *MemoryReceiptStore
	storeAtPost    *Receipt
	postBody       string
	postKey        string
	firstGatewayOK bool
}

func (f *recoveryRunnerServer) handler(t *testing.T) http.Handler {
	auth := f.auth.handler()
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/api/mobile/v1/auth/challenge",
			request.URL.Path == "/api/mobile/v1/auth/session",
			request.URL.Path == "/api/mobile/v1/installations":
			auth.ServeHTTP(writer, request)
		case request.Method == http.MethodGet && request.URL.Path == "/api/mobile/v1/me":
			binding := "1"
			body := meBody("7", &binding, "active")
			body = strings.Replace(body, `"data_access":"onboarding_hour"`, `"data_access":"subscription_data"`, 1)
			body = strings.Replace(body, `"effective_deadline":"2026-09-21T11:00:00Z"`, `"effective_deadline":"2027-09-28T09:00:00Z"`, 1)
			_, _ = io.WriteString(writer, body)
		case request.Method == http.MethodGet && request.URL.Path == "/api/mobile/v1/gateways":
			f.mu.Lock()
			f.gatewayCalls++
			call := f.gatewayCalls
			tokens := f.gatewayTokens
			f.mu.Unlock()
			if call == 1 && tokens != "" {
				writer.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(writer, `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"ACCESS_SYNC_PENDING","retryable":true,"details":{"catalog_revision":"`+tokens+`","binding_revision":"1"}}`)
				return
			}
			_, _ = io.WriteString(writer, catalogBody("20", 1))
		case request.Method == http.MethodPost && request.URL.Path == "/api/mobile/v1/access/sync":
			raw, _ := io.ReadAll(request.Body)
			f.mu.Lock()
			f.events = append(f.events, "POST /api/mobile/v1/access/sync?observe=none")
			if f.store != nil && f.store.Value != nil {
				snapshot := *f.store.Value
				f.storeAtPost = &snapshot
			}
			f.postKey = request.Header.Get("Idempotency-Key")
			f.postBody = string(raw)
			response := f.syncResponse
			cut := f.cutSync
			f.mu.Unlock()
			if cut {
				if hijacker, ok := writer.(http.Hijacker); ok {
					conn, _, err := hijacker.Hijack()
					if err == nil {
						_ = conn.Close()
						return
					}
				}
				panic("no hijacker")
			}
			_, _ = io.WriteString(writer, response)
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/mobile/v1/operations/"):
			query := ""
			if request.URL.RawQuery != "" {
				query = "?" + request.URL.RawQuery
			}
			f.mu.Lock()
			f.events = append(f.events, "GET "+request.URL.Path+query)
			body := f.operationBody
			f.mu.Unlock()
			_, _ = io.WriteString(writer, body)
		default:
			auth.ServeHTTP(writer, request)
		}
	})
}

func (f *recoveryRunnerServer) postSnapshot() (string, string, *Receipt) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postKey, f.postBody, f.storeAtPost
}

func (f *recoveryRunnerServer) eventSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func newRecoveryRunnerServer(t *testing.T, gatewayTokens, marker string, syncResponse string) (*recoveryRunnerServer, *httptest.Server) {
	t.Helper()
	auth := newAuthFixture(t)
	auth.installations[InstallationFingerprint(auth.spkiDER)] = true
	auth.linked = true
	fixture := &recoveryRunnerServer{
		auth:          auth,
		gatewayTokens: gatewayTokens,
		operationBody: marker,
		syncResponse:  syncResponse,
		store:         &MemoryReceiptStore{},
	}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	return fixture, server
}

func seedOldReceipt(t *testing.T, session *MobileSession, store *MemoryReceiptStore, operationID string) string {
	t.Helper()
	body := AccessSyncRequest{CatalogRevision: "19", BindingRevision: "1"}
	digest, err := body.Digest()
	if err != nil {
		t.Fatal(err)
	}
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store.Value = &Receipt{
		Subject:    session.Subject(),
		Key:        key,
		BodyDigest: digest,
		Request:    body,
		Response: &AccessSyncResponse{
			RequestID:              "0123456789abcdef0123456789abcdef",
			ServerTime:             "2026-09-21T12:00:00Z",
			SchemaVersion:          "1.0",
			Status:                 "ok",
			OperationID:            operationID,
			AccessApplicationState: "retryable_failure",
		},
	}
	return key
}

func newRecoveryHarness(t *testing.T, fixture *recoveryRunnerServer, server *httptest.Server, oldOperationID string) (*Runner, *MobileSession, string) {
	t.Helper()
	session := newFixtureSession(t, fixture.auth, server)
	if err := session.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !containsString(session.Scopes(), "access:sync") {
		t.Fatalf("linked session must carry access:sync: %v", session.Scopes())
	}
	oldKey := seedOldReceipt(t, session, fixture.store, oldOperationID)
	coordinator, err := NewCoordinator(Options{
		Client:    &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session},
		AttemptID: "attempt",
		Store:     fixture.store,
		Subject:   session.Subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{
		Session:        session,
		Coordinator:    coordinator,
		Attempts:       3,
		RefreshFloor:   time.Minute,
		Jitter:         func(time.Duration) time.Duration { return 0 },
		Sleep:          func(context.Context, time.Duration) error { return nil },
		SelectedNodeID: func() string { return "" },
		Emit:           func(context.Context, map[string]any) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner, session, oldKey
}

func newRecoveryRunnerFromStore(t *testing.T, fixture *recoveryRunnerServer, server *httptest.Server) *Runner {
	t.Helper()
	session := newFixtureSession(t, fixture.auth, server)
	if err := session.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(Options{
		Client:    &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session},
		AttemptID: "attempt",
		Store:     fixture.store,
		Subject:   session.Subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{
		Session:        session,
		Coordinator:    coordinator,
		Attempts:       3,
		RefreshFloor:   time.Minute,
		Jitter:         func(time.Duration) time.Duration { return 0 },
		Sleep:          func(context.Context, time.Duration) error { return nil },
		SelectedNodeID: func() string { return "" },
		Emit:           func(context.Context, map[string]any) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestRunnerRecoveryProbesBeforeFirstPostAndPersistsNewKey(t *testing.T) {
	marker := fenceOperationBody("op-1", "retryable_failure", "",
		`{"required":true,"catalog_revision":"20","binding_revision":"1"}`)
	fixture, server := newRecoveryRunnerServer(t, "20", marker, syncBody("op-2", "retryable_failure"))
	runner, _, oldKey := newRecoveryHarness(t, fixture, server, "op-1")

	if _, _, err := runner.cycle(context.Background()); err != nil {
		t.Fatalf("runner cycle: %v", err)
	}
	events := fixture.eventSnapshot()
	if len(events) != 2 ||
		events[0] != "GET /api/mobile/v1/operations/op-1?observe=sync_recovery" ||
		events[1] != "POST /api/mobile/v1/access/sync?observe=none" {
		t.Fatalf("the opt-in GET must precede the only POST: %v", events)
	}
	postKey, postBody, storeAtPost := fixture.postSnapshot()
	if storeAtPost == nil {
		t.Fatal("no receipt snapshot captured at POST time")
	}
	if storeAtPost.Key == oldKey {
		t.Fatalf("rotated POST must carry a new key: %s", storeAtPost.Key)
	}
	if storeAtPost.Response != nil {
		t.Fatalf("the new key/body must be persisted BEFORE the POST: %+v", storeAtPost)
	}
	if storeAtPost.Request.CatalogRevision != "20" || storeAtPost.Request.BindingRevision != "1" {
		t.Fatalf("rotated body must carry the marker revisions: %+v", storeAtPost.Request)
	}
	if postKey != storeAtPost.Key {
		t.Fatalf("POST key %s must equal the persisted key", postKey)
	}
	_ = postBody
	if fixture.store.Value == nil || fixture.store.Value.Response == nil ||
		fixture.store.Value.Response.OperationID != "op-2" || fixture.store.Value.Key == oldKey {
		t.Fatalf("durable receipt after the cycle: %+v", fixture.store.Value)
	}
}

func TestRunnerRecoveryLostResponseRestartResumesNewKeyWithoutProbe(t *testing.T) {
	marker := fenceOperationBody("op-1", "retryable_failure", "",
		`{"required":true,"catalog_revision":"20","binding_revision":"1"}`)
	fixture, server := newRecoveryRunnerServer(t, "20", marker, syncBody("op-2", "retryable_failure"))
	runner, _, oldKey := newRecoveryHarness(t, fixture, server, "op-1")

	fixture.mu.Lock()
	fixture.cutSync = true
	fixture.mu.Unlock()
	if _, _, err := runner.cycle(context.Background()); err == nil {
		t.Fatal("cut rotated response must surface as a transport error")
	}
	pending := *fixture.store.Value
	if pending.Key == oldKey || pending.Response != nil {
		t.Fatalf("lost rotated response must keep the NEW pending key: %+v", pending)
	}
	firstPostKey, firstPostBody, _ := fixture.postSnapshot()
	if firstPostBody == "" || firstPostKey != pending.Key {
		t.Fatalf("the cut POST must already carry the persisted new key/body: key=%q body=%q", firstPostKey, firstPostBody)
	}

	fixture.mu.Lock()
	fixture.cutSync = false
	fixture.events = nil
	fixture.mu.Unlock()
	restarted := newRecoveryRunnerFromStore(t, fixture, server)
	if _, _, err := restarted.cycle(context.Background()); err != nil {
		t.Fatalf("restart cycle: %v", err)
	}
	events := fixture.eventSnapshot()
	if len(events) != 1 || events[0] != "POST /api/mobile/v1/access/sync?observe=none" {
		t.Fatalf("a lost-response restart must resend without a probe: %v", events)
	}
	secondKey, secondBody, _ := fixture.postSnapshot()
	if secondKey != pending.Key || secondBody != firstPostBody {
		t.Fatalf("restart must resend the persisted new key/body: key=%q body=%q want key=%q body=%q",
			secondKey, secondBody, pending.Key, firstPostBody)
	}
	if !strings.Contains(secondBody, `"catalog_revision":"20"`) ||
		!strings.Contains(secondBody, `"binding_revision":"1"`) {
		t.Fatalf("resumed body must match the persisted 20/1 request: %s", secondBody)
	}
	if fixture.store.Value == nil || fixture.store.Value.Key != pending.Key ||
		fixture.store.Value.Request.CatalogRevision != "20" ||
		fixture.store.Value.Request.BindingRevision != "1" {
		t.Fatalf("no second new key may be minted and the persisted request stays 20/1: %+v", fixture.store.Value)
	}
}

func TestRunnerRecoveryPendingTransitionNeverPosts(t *testing.T) {
	pending := fenceOperationBody("op-1", "pending", "", "")
	fixture, server := newRecoveryRunnerServer(t, "20", pending, syncBody("op-2", "retryable_failure"))
	runner, _, oldKey := newRecoveryHarness(t, fixture, server, "op-1")

	if _, _, err := runner.cycle(context.Background()); err != nil {
		t.Fatalf("runner cycle: %v", err)
	}
	for _, event := range fixture.eventSnapshot() {
		if strings.HasPrefix(event, "POST") {
			t.Fatalf("a pending operation must never receive a POST: %v", fixture.eventSnapshot())
		}
	}
	if fixture.store.Value == nil || fixture.store.Value.Key != oldKey ||
		fixture.store.Value.Response == nil ||
		fixture.store.Value.Response.AccessApplicationState != "pending" {
		t.Fatalf("pending transition must keep the same key and durable state: %+v", fixture.store.Value)
	}
}

func TestRunnerRecoveryTerminalTransitionSameDigestNeverPosts(t *testing.T) {
	applied := fenceOperationBody("op-1", "applied", "", "")
	fixture, server := newRecoveryRunnerServer(t, "19", applied, syncBody("op-2", "applied"))
	runner, _, oldKey := newRecoveryHarness(t, fixture, server, "op-1")

	if _, _, err := runner.cycle(context.Background()); err != nil {
		t.Fatalf("runner cycle: %v", err)
	}
	for _, event := range fixture.eventSnapshot() {
		if strings.HasPrefix(event, "POST") {
			t.Fatalf("a terminal same-digest operation must never receive a stale POST: %v", fixture.eventSnapshot())
		}
	}
	if fixture.store.Value == nil || fixture.store.Value.Key != oldKey {
		t.Fatalf("terminal replay must keep the persisted key: %+v", fixture.store.Value)
	}
}

package accountaccess

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Runner tests use the accepted PoP/auth fixture for the session and controlled
// /me, /gateways, /access/sync and /operations fixtures for the coordinator.

type runnerFixture struct {
	mu             sync.Mutex
	meCalls        int
	catalogCalls   int
	syncCalls      int
	operationCalls int
	auth           *authFixture
	failMe         bool
	syncResponse   string
	gatewayBody    string
	gatewayStatus  int
}

func (f *runnerFixture) handler(t *testing.T) http.Handler {
	auth := f.auth.handler()
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/mobile/v1/me":
			f.mu.Lock()
			f.meCalls++
			fail := f.failMe
			f.mu.Unlock()
			if fail {
				writer.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(writer, `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"SUBSCRIPTION_MISSING","retryable":false}`)
				return
			}
			binding := "1"
			body := meBody("7", &binding, "active")
			body = strings.Replace(body, `"data_access":"onboarding_hour"`, `"data_access":"subscription_data"`, 1)
			body = strings.Replace(body, `"effective_deadline":"2026-09-21T11:00:00Z"`, `"effective_deadline":"2027-09-28T09:00:00Z"`, 1)
			_, _ = io.WriteString(writer, body)
		case "/api/mobile/v1/gateways":
			f.mu.Lock()
			f.catalogCalls++
			override, status := f.gatewayBody, f.gatewayStatus
			f.mu.Unlock()
			body := strings.Replace(catalogBody("4", 1),
				`"valid_until":"2026-09-21T12:10:00Z"`, `"valid_until":"2027-01-01T00:00:00Z"`, 1)
			if override != "" {
				body = override
			}
			if status != 0 {
				writer.WriteHeader(status)
			}
			_, _ = io.WriteString(writer, body)
		case "/api/mobile/v1/access/sync":
			f.mu.Lock()
			f.syncCalls++
			response := f.syncResponse
			f.mu.Unlock()
			if response == "" {
				response = syncBody("op-1", "applied")
			}
			_, _ = io.WriteString(writer, response)
		case "/api/mobile/v1/operations/op-1":
			f.mu.Lock()
			f.operationCalls++
			f.mu.Unlock()
			_, _ = io.WriteString(writer, operationBody("op-1", "applied"))
		default:
			auth.ServeHTTP(writer, request)
		}
	})
}

func newRunnerFixture(t *testing.T, linked bool) (*runnerFixture, *httptest.Server) {
	t.Helper()
	auth := newAuthFixture(t)
	auth.installations[InstallationFingerprint(auth.spkiDER)] = true
	auth.linked = linked
	fixture := &runnerFixture{auth: auth}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	return fixture, server
}

func newRunner(t *testing.T, fixture *runnerFixture, server *httptest.Server, onError func(string), onAdmission func(AdmissionDecision)) (*Runner, *MobileSession, *[]map[string]any) {
	t.Helper()
	session := newFixtureSession(t, fixture.auth, server)
	coordinator, err := NewCoordinator(Options{
		Client:    &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session},
		AttemptID: "attempt",
		Store:     &MemoryReceiptStore{},
		Subject:   session.Subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	emitted := &[]map[string]any{}
	var emittedMu sync.Mutex
	runner, err := NewRunner(RunnerConfig{
		Session:        session,
		Coordinator:    coordinator,
		Attempts:       3,
		RefreshFloor:   time.Minute,
		Jitter:         func(time.Duration) time.Duration { return 0 },
		Sleep:          func(context.Context, time.Duration) error { return nil },
		OnError:        onError,
		OnAdmission:    onAdmission,
		SelectedNodeID: func() string { return "" },
		Emit: func(ctx context.Context, payload map[string]any) error {
			emittedMu.Lock()
			defer emittedMu.Unlock()
			*emitted = append(*emitted, payload)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner, session, emitted
}

func TestRunnerEmitsAndRefreshesOnWakeTrigger(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	runner, _, emitted := newRunner(t, fixture, server, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = runner.Run(ctx)
		close(done)
	}()
	waitFor(t, func() bool {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		return fixture.meCalls >= 1
	})
	runner.Trigger("wake")
	waitFor(t, func() bool {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		return fixture.meCalls >= 2
	})
	cancel()
	<-done
	if len(*emitted) == 0 {
		t.Fatal("runner must emit the accepted projection")
	}
}

func TestRunnerEmitsPreAdmissionMeBeforeManagementOnlyCatalogDenial(t *testing.T) {
	auth := newAuthFixture(t)
	auth.installations[InstallationFingerprint(auth.spkiDER)] = true
	var mu sync.Mutex
	meCalls, catalogCalls, intentCalls := 0, 0, 0
	authHandler := auth.handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/api/mobile/v1/me":
			mu.Lock()
			meCalls++
			mu.Unlock()
			body := meBody("7", nil, "not_started")
			body = strings.Replace(body, `"account_state":"ACTIVE_TRIAL"`, `"account_state":"UNLINKED"`, 1)
			body = strings.Replace(body, `"binding_status":"active"`, `"binding_status":"none"`, 1)
			body = strings.Replace(body, `"management_only":false`, `"management_only":true`, 1)
			_, _ = io.WriteString(w, body)
		case "/api/mobile/v1/gateways":
			mu.Lock()
			catalogCalls++
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"ACCESS_DENIED","retryable":false}`)
		default:
			if strings.Contains(req.URL.Path, "onboarding") {
				mu.Lock()
				intentCalls++
				mu.Unlock()
			}
			authHandler.ServeHTTP(w, req)
		}
	}))
	defer server.Close()
	session := newFixtureSession(t, auth, server)
	coordinator, err := NewCoordinator(Options{
		Client:    &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session},
		AttemptID: "attempt", Store: &MemoryReceiptStore{}, Subject: session.Subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	var emitted []map[string]any
	runner, err := NewRunner(RunnerConfig{
		Session: session, Coordinator: coordinator,
		Emit:           func(_ context.Context, payload map[string]any) error { emitted = append(emitted, payload); return nil },
		SelectedNodeID: func() string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog, browse, err := runner.cycle(context.Background())
	if err != nil || catalog != nil || browse != nil {
		t.Fatalf("management-only catalog must not project data: catalog=%v browse=%v err=%v", catalog, browse, err)
	}
	if len(emitted) != 1 {
		t.Fatalf("/me projection count = %d, want 1 before catalog denial", len(emitted))
	}
	account := emitted[0]["account"].(map[string]any)
	onboarding := emitted[0]["onboarding"].(map[string]any)
	grant := emitted[0]["grant_resolution"].(map[string]any)
	if account["management_only"] != true || onboarding["state"] != "not_started" || grant["data_access"] != "none" {
		t.Fatal("pre-admission projection not emitted before forbidden catalog")
	}
	mu.Lock()
	defer mu.Unlock()
	if meCalls != 1 || catalogCalls != 1 || intentCalls != 0 {
		t.Fatalf("unexpected background operations: me=%d catalog=%d intents=%d", meCalls, catalogCalls, intentCalls)
	}
}

func TestRunnerStopsOnNonRetryableApiErrorWithoutSpinning(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	fixture.failMe = true
	errors := make(chan string, 4)
	runner, _, _ := newRunner(t, fixture, server, func(code string) { errors <- code }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = runner.Run(ctx)
		close(done)
	}()
	select {
	case code := <-errors:
		if code != "SUBSCRIPTION_MISSING" {
			t.Fatalf("safe error code wrong: %s", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not report the non-retryable API error")
	}
	time.Sleep(50 * time.Millisecond)
	fixture.mu.Lock()
	calls := fixture.meCalls
	fixture.mu.Unlock()
	if calls != 1 {
		t.Fatalf("non-retryable error must not spin: meCalls=%d", calls)
	}
	cancel()
	<-done
}

func TestRunnerPollsPendingOperationToTerminal(t *testing.T) {
	fixture, server := newRunnerFixture(t, true)
	fixture.syncResponse = syncBody("op-1", "pending")
	admissions := make(chan AdmissionDecision, 2)
	runner, session, _ := newRunner(t, fixture, server, nil, func(decision AdmissionDecision) { admissions <- decision })
	if err := session.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !containsString(session.Scopes(), "access:sync") {
		t.Fatalf("linked session must carry access:sync: %v", session.Scopes())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = runner.Run(ctx)
		close(done)
	}()
	waitFor(t, func() bool {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		return fixture.operationCalls >= 1
	})
	cancel()
	<-done
	if fixture.syncCalls == 0 {
		t.Fatal("sync was not attempted")
	}
}

// TestRunnerBrowseNeverCallsOnVerified proves the runner routes the display branch
// exclusively through OnBrowse: the verified-pair callback and the access/sync path
// are never reached for a browse response.
func TestRunnerBrowseNeverCallsOnVerified(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	runner, _, _ := newRunner(t, fixture, server, nil, nil)
	var verified, browsed int
	runner.config.OnVerified = func(MeResponse, CatalogResponse) { verified++ }
	runner.config.OnBrowse = func(BrowseCatalogResponse) { browsed++ }
	fixture.mu.Lock()
	fixture.gatewayBody = browseBody(3)
	fixture.mu.Unlock()

	catalog, browse, err := runner.cycle(context.Background())
	if err != nil || catalog != nil || browse == nil || len(browse.Gateways) != 3 {
		t.Fatalf("browse cycle wrong: catalog=%v browse=%+v err=%v", catalog, browse, err)
	}
	if verified != 0 || browsed != 1 {
		t.Fatalf("browse must not reach OnVerified: verified=%d browsed=%d", verified, browsed)
	}
	fixture.mu.Lock()
	syncCalls := fixture.syncCalls
	fixture.mu.Unlock()
	if syncCalls != 0 {
		t.Fatalf("browse must not reach access/sync: %d", syncCalls)
	}
}

// TestRunnerAdmissionPendingStillSyncsWithoutCatalog locks the accepted pre-catalog
// admission flow: a 409 ACCESS_SYNC_PENDING with admission tokens must still reach
// access/sync, and the browse support must not have replaced it.
func TestRunnerAdmissionPendingStillSyncsWithoutCatalog(t *testing.T) {
	fixture, server := newRunnerFixture(t, true)
	runner, _, _ := newRunner(t, fixture, server, nil, nil)
	var verified int
	runner.config.OnVerified = func(MeResponse, CatalogResponse) { verified++ }
	fixture.mu.Lock()
	fixture.gatewayStatus = http.StatusConflict
	fixture.gatewayBody = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z",` +
		`"schema_version":"1.0","status":"error","code":"ACCESS_SYNC_PENDING","retryable":true,` +
		`"retry_after_ms":1500,"details":{"catalog_revision":"5","binding_revision":"1"}}`
	fixture.mu.Unlock()

	// A still-pending catalog after the sync is a bounded retry, never a hot loop: the
	// existing Attempts/RetryDelay budget must be used and a canceled context must stop it.
	var sleeps []time.Duration
	runner.config.Jitter = func(delay time.Duration) time.Duration { return delay }
	runner.config.Sleep = func(_ context.Context, delay time.Duration) error {
		sleeps = append(sleeps, delay)
		return nil
	}

	catalog, browse, wait, err := runner.attempt(context.Background())
	if err == nil || catalog != nil || browse != nil {
		t.Fatalf("pending admission cycle wrong: catalog=%v browse=%v err=%v", catalog, browse, err)
	}
	if !runnerRetryable(err) {
		t.Fatalf("pending admission must stay retryable: %v", err)
	}
	if len(sleeps) != runner.config.Attempts-1 {
		t.Fatalf("bounded retries wrong: sleeps=%d attempts=%d", len(sleeps), runner.config.Attempts)
	}
	// Existing RetryDelay backoff: attempt 1 -> 2s, attempt 2 -> 4s for the plain retryable error.
	if !equalDurations(sleeps, []time.Duration{2 * time.Second, 4 * time.Second}) {
		t.Fatalf("retry delays wrong: %v", sleeps)
	}
	if wait != 2*time.Second {
		t.Fatalf("retry wait must follow RetryDelay: %v", wait)
	}
	fixture.mu.Lock()
	syncCalls := fixture.syncCalls
	fixture.mu.Unlock()
	if syncCalls == 0 {
		t.Fatal("admission-pending tokens must still authorize access/sync")
	}
	if verified != 0 {
		t.Fatalf("no verified catalog exists yet: verified=%d", verified)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	runner.config.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if _, _, _, err := runner.attempt(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retry must return the context error: %v", err)
	}
}

func TestRunnerAdmissionCallbackReportsRemovedSelection(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	decisions := make(chan AdmissionDecision, 2)
	runner, _, _ := newRunner(t, fixture, server, nil, func(decision AdmissionDecision) { decisions <- decision })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The selected node is not in the verified catalog: the callback must prove removal.
	runner.config.SelectedNodeID = func() string { return "missing-node" }
	go func() { _ = runner.Run(ctx) }()
	select {
	case decision := <-decisions:
		if !decision.StopDataPlane || decision.Reason != "SELECTED_NODE_REMOVED" {
			t.Fatalf("removed selection must stop the data plane: %+v", decision)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("admission callback not invoked")
	}
	cancel()
}

func TestRunnerOnVerifiedCarriesTheAcceptedPairBeforeAdmission(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	session := newFixtureSession(t, fixture.auth, server)
	coordinator, err := NewCoordinator(Options{
		Client:    &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session},
		AttemptID: "attempt",
		Store:     &MemoryReceiptStore{},
		Subject:   session.Subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	type verifiedPair struct {
		me      MeResponse
		catalog CatalogResponse
	}
	verified := make(chan verifiedPair, 2)
	runner, err := NewRunner(RunnerConfig{
		Session:        session,
		Coordinator:    coordinator,
		Emit:           func(context.Context, map[string]any) error { return nil },
		OnVerified:     func(me MeResponse, catalog CatalogResponse) { verified <- verifiedPair{me: me, catalog: catalog} },
		SelectedNodeID: func() string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	select {
	case pair := <-verified:
		if pair.me.Revision != "7" || pair.catalog.Revision != "4" {
			t.Fatalf("verified pair wrong: me=%s catalog=%s", pair.me.Revision, pair.catalog.Revision)
		}
		if len(pair.catalog.Gateways) != 1 || pair.catalog.Gateways[0].GatewayID != "gw-synth-0" {
			t.Fatal("verified catalog shape wrong")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnVerified was not called for the accepted pair")
	}
	cancel()
}

func equalDurations(got, want []time.Duration) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

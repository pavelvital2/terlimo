package accountaccess

// S5-074 connected renewal: while Connected with an unchanged admissible body, the
// backend renews the node lease only for an authenticated POST /access/sync
// (ensure_grant). These tests drive the real Coordinator/Runner over an httptest
// fixture with a fake runner clock and assert: exactly one forced renewal per proven
// horizon, the same body with a new durable key persisted before the POST, the existing
// pending/poll/refresh machinery extending the horizon, no second key while a renewal is
// pending or failed, and the fail-closed stop at the true deadline. No real network,
// device, build or server is touched.

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

func renewalStamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

// renewalMeBody is the accepted subscription_data snapshot with a controlled effective
// deadline; the binding stays active so DecideAdmission can admit the paid/trial right.
func renewalMeBody(revision, deadline string) string {
	binding := "1"
	body := meBody(revision, &binding, "active")
	body = strings.Replace(body, `"data_access":"onboarding_hour"`, `"data_access":"subscription_data"`, 1)
	body = strings.Replace(body, `"effective_deadline":"2026-09-21T11:00:00Z"`, `"effective_deadline":"`+deadline+`"`, 1)
	return body
}

func renewalRevokedMeBody(revision, deadline string) string {
	body := renewalMeBody(revision, deadline)
	return strings.Replace(body, `"entitlement":{"type":"trial","status":"active"`, `"entitlement":{"type":"trial","status":"revoked"`, 1)
}

func renewalCatalogBody(revision, issuedAt, validUntil, notAfter string) string {
	body := catalogBody(revision, 1)
	body = strings.Replace(body, `"issued_at":"2026-09-21T11:55:00Z"`, `"issued_at":"`+issuedAt+`"`, 1)
	body = strings.Replace(body, `"valid_until":"2026-09-21T12:10:00Z"`, `"valid_until":"`+validUntil+`"`, 1)
	body = strings.Replace(body, `"not_after":"2026-09-21T12:10:00Z"`, `"not_after":"`+notAfter+`"`, 1)
	return body
}

func renewalDecodeMe(t *testing.T, body string) MeResponse {
	t.Helper()
	me, err := DecodeMeStrict([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return me
}

type renewalClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *renewalClock) get() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *renewalClock) set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

// renewalFixture is a stateful /me + /gateways + /access/sync + /operations backend. The
// catalog it serves switches to renewedCatalogBody only once the armed renewal POST has
// reached the applied state (through its own response or a poll), mirroring the backend
// applying ensure_grant before re-issuing the same-revision snapshot.
type renewalFixture struct {
	auth *authFixture

	mu             sync.Mutex
	meBody         string
	catalogBody    string
	renewedCatalog string
	renewFromSync  int
	renewApplied   bool
	operationBody  string
	syncStatuses   []int
	syncResponses  []string
	syncKeys       []string
	syncRequests   []string
	syncCalls      int
	meCalls        int
	gatewayCalls   int
	operationCalls int
}

func newRenewalFixture(t *testing.T, meBody, catalogBody string) *renewalFixture {
	t.Helper()
	auth := newAuthFixture(t)
	auth.installations[InstallationFingerprint(auth.spkiDER)] = true
	auth.linked = true
	return &renewalFixture{
		auth:          auth,
		meBody:        meBody,
		catalogBody:   catalogBody,
		operationBody: operationBody("op-1", "applied"),
	}
}

func (f *renewalFixture) handler() http.Handler {
	auth := f.auth.handler()
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/api/mobile/v1/me":
			f.mu.Lock()
			f.meCalls++
			body := f.meBody
			f.mu.Unlock()
			_, _ = io.WriteString(writer, body)
		case request.URL.Path == "/api/mobile/v1/gateways":
			f.mu.Lock()
			f.gatewayCalls++
			body := f.catalogBody
			if f.renewFromSync > 0 && f.renewApplied && f.syncCalls >= f.renewFromSync && f.renewedCatalog != "" {
				body = f.renewedCatalog
			}
			f.mu.Unlock()
			_, _ = io.WriteString(writer, body)
		case request.URL.Path == "/api/mobile/v1/access/sync":
			raw, _ := io.ReadAll(request.Body)
			f.mu.Lock()
			index := f.syncCalls
			f.syncCalls++
			f.syncKeys = append(f.syncKeys, request.Header.Get("Idempotency-Key"))
			f.syncRequests = append(f.syncRequests, string(raw))
			status, body := http.StatusOK, syncBody("op-1", "applied")
			if index < len(f.syncStatuses) && f.syncStatuses[index] != 0 {
				status = f.syncStatuses[index]
			}
			if index < len(f.syncResponses) && f.syncResponses[index] != "" {
				body = f.syncResponses[index]
			}
			if status == http.StatusOK && f.renewFromSync > 0 && f.syncCalls >= f.renewFromSync &&
				strings.Contains(body, `"access_application_state":"applied"`) {
				f.renewApplied = true
			}
			f.mu.Unlock()
			writer.WriteHeader(status)
			_, _ = io.WriteString(writer, body)
		case strings.HasPrefix(request.URL.Path, "/api/mobile/v1/operations/"):
			f.mu.Lock()
			f.operationCalls++
			body := f.operationBody
			if f.renewFromSync > 0 && f.syncCalls >= f.renewFromSync &&
				strings.Contains(body, `"access_application_state":"applied"`) {
				f.renewApplied = true
			}
			f.mu.Unlock()
			_, _ = io.WriteString(writer, body)
		default:
			auth.ServeHTTP(writer, request)
		}
	})
}

func (f *renewalFixture) setMe(body string) {
	f.mu.Lock()
	f.meBody = body
	f.mu.Unlock()
}

func (f *renewalFixture) setCatalog(body string) {
	f.mu.Lock()
	f.catalogBody = body
	f.renewedCatalog = ""
	f.renewFromSync = 0
	f.renewApplied = false
	f.mu.Unlock()
}

func (f *renewalFixture) setOperation(body string) {
	f.mu.Lock()
	f.operationBody = body
	f.mu.Unlock()
}

func (f *renewalFixture) setSyncResponse(index, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.syncStatuses) <= index {
		f.syncStatuses = append(f.syncStatuses, 0)
	}
	for len(f.syncResponses) <= index {
		f.syncResponses = append(f.syncResponses, "")
	}
	f.syncStatuses[index] = status
	f.syncResponses[index] = body
}

// armRenewal makes renewedCatalogBody visible once the fromSync-th POST (or a poll after
// it) has reached the applied state.
func (f *renewalFixture) armRenewal(fromSync int, renewed string) {
	f.mu.Lock()
	f.renewFromSync = fromSync
	f.renewedCatalog = renewed
	f.renewApplied = false
	f.mu.Unlock()
}

func (f *renewalFixture) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.syncKeys)
}

func (f *renewalFixture) syncKeysCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.syncKeys...)
}

func (f *renewalFixture) syncRequestsCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.syncRequests...)
}

func newConnectedRenewalRunner(t *testing.T, fixture *renewalFixture, clock *renewalClock) (*Runner, *Coordinator, *MemoryReceiptStore) {
	t.Helper()
	server := httptest.NewServer(fixture.handler())
	t.Cleanup(server.Close)
	session := newFixtureSession(t, fixture.auth, server)
	store := &MemoryReceiptStore{}
	coordinator, err := NewCoordinator(Options{
		Client:    &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session},
		AttemptID: "attempt",
		Store:     store,
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
		Emit:           func(context.Context, map[string]any) error { return nil },
		SelectedNodeID: func() string { return "gw-synth-0" },
		OnAdmission:    func(AdmissionDecision) {},
		Now:            clock.get,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner, coordinator, store
}

func TestRunnerConnectedRenewalBeforeDeadlineExtendsHorizon(t *testing.T) {
	now0 := time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)
	deadline := now0.Add(600 * time.Second)
	extended := deadline.Add(600 * time.Second)
	meBody := renewalMeBody("7", renewalStamp(now0.Add(24*time.Hour)))
	fixture := newRenewalFixture(t, meBody, renewalCatalogBody("90", renewalStamp(now0.Add(-5*time.Minute)), renewalStamp(deadline), renewalStamp(deadline)))
	clock := &renewalClock{now: now0}
	runner, _, store := newConnectedRenewalRunner(t, fixture, clock)

	initial, browse, err := runner.cycle(context.Background())
	if err != nil || browse != nil || initial == nil {
		t.Fatalf("initial cycle: catalog=%v browse=%v err=%v", initial, browse, err)
	}
	if fixture.syncCount() != 1 {
		t.Fatalf("initial authorization must POST exactly once: %d", fixture.syncCount())
	}
	firstKey := store.Value.Key
	if firstKey == "" || store.Value.Response == nil || store.Value.Response.AccessApplicationState != "applied" {
		t.Fatalf("initial receipt must be the applied authorization: %+v", store.Value)
	}
	// Far from the horizon only the healthy catalog refresh is scheduled.
	if next := runner.nextCycleAt(*initial); !next.Equal(deadline.Add(-renewLead)) {
		t.Fatalf("initial wait %v, want the renewal lead point %v", next, deadline.Add(-renewLead))
	}

	// The renewal response is pending; the refreshed (same-revision, newer issued_at)
	// snapshot becomes available once the renewal is applied.
	fixture.setSyncResponse(1, http.StatusOK, syncBody("op-1", "pending"))
	fixture.armRenewal(2, renewalCatalogBody("90", renewalStamp(deadline.Add(-time.Minute)), renewalStamp(extended), renewalStamp(extended)))

	clock.set(deadline.Add(-239 * time.Second))
	renewed, browse, err := runner.cycle(context.Background())
	if err != nil || browse != nil || renewed == nil {
		t.Fatalf("renewal cycle: catalog=%v browse=%v err=%v", renewed, browse, err)
	}
	if fixture.syncCount() != 2 {
		t.Fatalf("exactly one forced renewal POST expected, got %d", fixture.syncCount())
	}
	keys := fixture.syncKeysCopy()
	if keys[1] == "" || keys[1] == keys[0] {
		t.Fatalf("forced renewal must mint a NEW durable key: %q -> %q", keys[0], keys[1])
	}
	requests := fixture.syncRequestsCopy()
	if requests[1] != requests[0] {
		t.Fatalf("forced renewal must carry the SAME admissible body: %q != %q", requests[0], requests[1])
	}
	if store.Value.Key != keys[1] || store.Value.Response == nil || store.Value.Response.AccessApplicationState != "applied" {
		t.Fatalf("renewal receipt must be the polled applied operation: %+v", store.Value)
	}
	if renewed.ValidUntil != renewalStamp(extended) {
		t.Fatalf("horizon not extended: %+v", renewed)
	}
	if next := runner.nextCycleAt(*renewed); !next.Equal(extended.Add(-renewLead)) {
		t.Fatalf("next renewal wake %v, want %v", next, extended.Add(-renewLead))
	}
	// The superseded deadline must not stop the extended horizon.
	if decision := DecideAdmission(renewalDecodeMe(t, meBody), *renewed, "gw-synth-0", deadline); !decision.Admitted {
		t.Fatalf("extended horizon must still admit at the old deadline: %+v", decision)
	}
}

func TestRunnerConnectedRenewalNoStormAndSingleKeyWhilePending(t *testing.T) {
	now0 := time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)
	deadline := now0.Add(10 * time.Minute)
	extended := deadline.Add(10 * time.Minute)
	second := extended.Add(10 * time.Minute)
	meBody := renewalMeBody("7", renewalStamp(now0.Add(24*time.Hour)))
	baseCatalog := renewalCatalogBody("90", renewalStamp(now0.Add(-5*time.Minute)), renewalStamp(deadline), renewalStamp(deadline))
	fixture := newRenewalFixture(t, meBody, baseCatalog)
	clock := &renewalClock{now: now0}
	runner, _, store := newConnectedRenewalRunner(t, fixture, clock)

	if _, _, err := runner.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.syncCount() != 1 {
		t.Fatalf("initial authorization count: %d", fixture.syncCount())
	}
	firstKey := store.Value.Key

	// Renewal #1 is pending and its poll stays pending: the horizon must NOT falsely
	// extend and no second key may be minted.
	fixture.setSyncResponse(1, http.StatusOK, syncBody("op-1", "pending"))
	fixture.setOperation(operationBody("op-1", "pending"))
	fixture.armRenewal(2, renewalCatalogBody("90", renewalStamp(deadline.Add(-time.Minute)), renewalStamp(extended), renewalStamp(extended)))
	clock.set(deadline.Add(-239 * time.Second))

	pendingCatalog, _, err := runner.cycle(context.Background())
	if err != nil || pendingCatalog == nil {
		t.Fatalf("pending renewal cycle: catalog=%v err=%v", pendingCatalog, err)
	}
	if fixture.syncCount() != 2 {
		t.Fatalf("exactly one forced renewal POST expected: %d", fixture.syncCount())
	}
	renewalKey := store.Value.Key
	if renewalKey == firstKey || renewalKey == "" {
		t.Fatalf("renewal key must be new: %q -> %q", firstKey, renewalKey)
	}
	if pendingCatalog.ValidUntil != renewalStamp(deadline) {
		t.Fatalf("pending renewal must not extend the horizon: %+v", pendingCatalog)
	}
	for round := 0; round < 3; round++ {
		catalog, _, err := runner.cycle(context.Background())
		if err != nil || catalog == nil {
			t.Fatalf("repeat cycle %d: catalog=%v err=%v", round, catalog, err)
		}
		if fixture.syncCount() != 2 {
			t.Fatalf("pending renewal must never mint a second key: %d POSTs", fixture.syncCount())
		}
		if store.Value.Key != renewalKey {
			t.Fatalf("pending renewal key changed: %q -> %q", renewalKey, store.Value.Key)
		}
		// Unresolved renewal keeps the bounded floor cadence, never a hot loop.
		if next := runner.nextCycleAt(*catalog); !next.Equal(clock.get().Add(runner.config.RefreshFloor)) {
			t.Fatalf("outstanding renewal wait %v, want floor %v", next, clock.get().Add(runner.config.RefreshFloor))
		}
	}

	// The operation becomes applied: the same cycle polls it and the refreshed catalog
	// extends the horizon; no new POST is made.
	fixture.setOperation(operationBody("op-1", "applied"))
	completed, _, err := runner.cycle(context.Background())
	if err != nil || completed == nil {
		t.Fatalf("completion cycle: catalog=%v err=%v", completed, err)
	}
	if fixture.syncCount() != 2 {
		t.Fatalf("completion must replay the pending receipt, not POST: %d", fixture.syncCount())
	}
	if completed.ValidUntil != renewalStamp(extended) {
		t.Fatalf("completed renewal must extend the horizon: %+v", completed)
	}
	if next := runner.nextCycleAt(*completed); !next.Equal(extended.Add(-renewLead)) {
		t.Fatalf("next renewal wake %v, want %v", next, extended.Add(-renewLead))
	}

	// Before the next lead point the unchanged horizon must replay without a POST.
	clock.set(extended.Add(-renewLead - time.Second))
	beforeLead, _, err := runner.cycle(context.Background())
	if err != nil || beforeLead == nil {
		t.Fatalf("before-lead cycle: catalog=%v err=%v", beforeLead, err)
	}
	if fixture.syncCount() != 2 {
		t.Fatalf("before the next lead no renewal POST is allowed: %d", fixture.syncCount())
	}

	// The next horizon renews exactly once at its own lead point.
	fixture.setCatalog(renewalCatalogBody("90", renewalStamp(deadline.Add(-time.Minute)), renewalStamp(extended), renewalStamp(extended)))
	fixture.armRenewal(3, renewalCatalogBody("90", renewalStamp(extended.Add(-time.Minute)), renewalStamp(second), renewalStamp(second)))
	clock.set(extended.Add(-renewLead + time.Second))
	next, _, err := runner.cycle(context.Background())
	if err != nil || next == nil {
		t.Fatalf("second renewal cycle: catalog=%v err=%v", next, err)
	}
	if fixture.syncCount() != 3 {
		t.Fatalf("second horizon must renew exactly once: %d POSTs", fixture.syncCount())
	}
	keys := fixture.syncKeysCopy()
	if keys[2] == keys[1] || keys[2] == "" {
		t.Fatalf("second renewal key must be new: %q -> %q", keys[1], keys[2])
	}
	if next.ValidUntil != renewalStamp(second) {
		t.Fatalf("second horizon not extended: %+v", next)
	}
	if wake := runner.nextCycleAt(*next); !wake.Equal(second.Add(-renewLead)) {
		t.Fatalf("third renewal wake %v, want %v", wake, second.Add(-renewLead))
	}
}

func TestRunnerConnectedRenewalFailureFailClosed(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status int
		body   string
	}{
		{"api_failure", http.StatusServiceUnavailable,
			`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"SERVICE_UNAVAILABLE","retryable":true}`},
		{"lost_response", http.StatusOK, `not-json`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			now0 := time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)
			deadline := now0.Add(10 * time.Minute)
			extended := deadline.Add(10 * time.Minute)
			meBody := renewalMeBody("7", renewalStamp(now0.Add(24*time.Hour)))
			catalogBody := renewalCatalogBody("90", renewalStamp(now0.Add(-5*time.Minute)), renewalStamp(deadline), renewalStamp(deadline))
			fixture := newRenewalFixture(t, meBody, catalogBody)
			clock := &renewalClock{now: now0}
			runner, coordinator, store := newConnectedRenewalRunner(t, fixture, clock)

			if _, _, err := runner.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			firstKey := store.Value.Key

			// Every bounded attempt fails: the renewal must never be reported applied.
			for index := 1; index <= 3; index++ {
				fixture.setSyncResponse(index, testCase.status, testCase.body)
			}
			fixture.armRenewal(2, renewalCatalogBody("90", renewalStamp(deadline.Add(-time.Minute)), renewalStamp(extended), renewalStamp(extended)))
			clock.set(deadline.Add(-239 * time.Second))

			// The bounded attempt budget runs out without ever applying an effect.
			catalog, browse, _, err := runner.attempt(context.Background())
			if err == nil || catalog != nil || browse != nil {
				t.Fatalf("failed renewal must return the transport error: catalog=%v browse=%v err=%v", catalog, browse, err)
			}
			keys := fixture.syncKeysCopy()
			if len(keys) < 2 {
				t.Fatalf("the forced renewal must have been attempted: %v", keys)
			}
			renewalKey := keys[1]
			if renewalKey == "" || renewalKey == firstKey {
				t.Fatalf("renewal key must be new: %q -> %q", firstKey, renewalKey)
			}
			for index, key := range keys[1:] {
				if key != renewalKey {
					t.Fatalf("failed/lost response must repeat the SAME key at %d: %q != %q", index+1, key, renewalKey)
				}
			}
			if store.Value.Key != renewalKey || store.Value.Response != nil {
				t.Fatalf("uncertain renewal must stay durable with the same key and no response: %+v", store.Value)
			}
			if lastGood := coordinator.LastGood(); lastGood == nil || lastGood.ValidUntil != renewalStamp(deadline) {
				t.Fatalf("failed renewal must not extend the horizon: %+v", lastGood)
			}
			// Unresolved renewal keeps the bounded floor cadence, never a hot loop.
			if next := runner.nextCycleAt(*coordinator.LastGood()); !next.Equal(clock.get().Add(runner.config.RefreshFloor)) {
				t.Fatalf("outstanding renewal wait %v, want floor %v", next, clock.get().Add(runner.config.RefreshFloor))
			}
			// At the true deadline the existing stop fence still fires fail-closed.
			decision := DecideAdmission(renewalDecodeMe(t, meBody), *coordinator.LastGood(), "gw-synth-0", deadline)
			if !decision.StopDataPlane || decision.Reason != "CATALOG_EXPIRED" {
				t.Fatalf("unrenewed horizon must stop at the true deadline: %+v", decision)
			}
		})
	}
}

func TestRunnerConnectedRenewalSkippedForRevokedRight(t *testing.T) {
	now0 := time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)
	deadline := now0.Add(10 * time.Minute)
	meBody := renewalMeBody("7", renewalStamp(now0.Add(24*time.Hour)))
	fixture := newRenewalFixture(t, meBody, renewalCatalogBody("90", renewalStamp(now0.Add(-5*time.Minute)), renewalStamp(deadline), renewalStamp(deadline)))
	clock := &renewalClock{now: now0}
	runner, _, store := newConnectedRenewalRunner(t, fixture, clock)

	if _, _, err := runner.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstKey := store.Value.Key

	var decisions []AdmissionDecision
	runner.config.OnAdmission = func(decision AdmissionDecision) { decisions = append(decisions, decision) }
	fixture.setMe(renewalRevokedMeBody("8", renewalStamp(now0.Add(24*time.Hour))))
	clock.set(deadline.Add(-239 * time.Second))

	if _, _, err := runner.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.syncCount() != 1 {
		t.Fatalf("revoked right must never start a renewal POST: %d", fixture.syncCount())
	}
	if store.Value.Key != firstKey {
		t.Fatalf("revoked right must keep the applied authorization receipt: %q -> %q", firstKey, store.Value.Key)
	}
	if len(decisions) == 0 {
		t.Fatal("revoked right must reach DecideAdmission")
	}
	last := decisions[len(decisions)-1]
	if !last.StopDataPlane || last.Reason != "ENTITLEMENT_REVOKED" {
		t.Fatalf("revoked entitlement must stop the data plane: %+v", last)
	}
}

func TestRenewalDeadlineTracksProvenMinimumAndShortenedHorizon(t *testing.T) {
	base := time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)
	effective := renewalStamp(base.Add(30 * time.Minute))
	me := MeResponse{GrantResolution: grantResolution{EffectiveDeadline: &effective}}
	catalog := CatalogResponse{
		ValidUntil: renewalStamp(base.Add(10 * time.Minute)),
		Gateways: []gateway{{
			GatewayID: "gw-a",
			Access:    accessDescriptor{NotAfter: renewalStamp(base.Add(5 * time.Minute))},
		}},
	}
	if deadline := renewalDeadline(me, catalog, "gw-a"); !deadline.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("lease must be the proven minimum: %v", deadline)
	}
	if deadline := renewalDeadline(me, catalog, "gw-b"); !deadline.Equal(base.Add(10 * time.Minute)) {
		t.Fatalf("another gateway lease must not participate: %v", deadline)
	}
	// The indefinite (null-deadline) shape keeps the catalog/lease bounds.
	me.GrantResolution.EffectiveDeadline = nil
	if deadline := renewalDeadline(me, catalog, "gw-a"); !deadline.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("null entitlement deadline must be skipped: %v", deadline)
	}

	// A shortened fresh horizon re-arms at its own shortened lead point with no max.
	runner := &Runner{}
	runner.renewedFor = base.Add(30 * time.Minute)
	shortDeadline := base.Add(5 * time.Minute)
	shortCatalog := CatalogResponse{
		ValidUntil: renewalStamp(shortDeadline),
		Gateways:   []gateway{{GatewayID: "gw-a", Access: accessDescriptor{NotAfter: renewalStamp(shortDeadline)}}},
	}
	plan := runner.renewalPlan(me, shortCatalog, "gw-a", true, shortDeadline.Add(-renewLead+time.Nanosecond))
	if !plan.due || plan.outstanding || !plan.deadline.Equal(shortDeadline) {
		t.Fatalf("shortened horizon must be due at its own lead: %+v", plan)
	}
	plan = runner.renewalPlan(me, shortCatalog, "gw-a", true, shortDeadline.Add(-renewLead-time.Nanosecond))
	if plan.due || plan.outstanding {
		t.Fatalf("before the shortened lead nothing is due: %+v", plan)
	}
	runner.renewedFor = shortDeadline
	plan = runner.renewalPlan(me, shortCatalog, "gw-a", true, shortDeadline.Add(-time.Second))
	if plan.due || !plan.outstanding {
		t.Fatalf("an attempted horizon stays outstanding until it changes: %+v", plan)
	}
	if plan := runner.renewalPlan(me, shortCatalog, "gw-a", false, shortDeadline.Add(-time.Second)); plan.due || plan.outstanding || !plan.deadline.IsZero() {
		t.Fatalf("a stopped right never plans a renewal: %+v", plan)
	}
}

func TestSyncAccessForcedMintsOneNewKeyForUnchangedBody(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, store := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("7", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.syncResponse = syncBody("op-1", "applied")

	initial, err := coordinator.SyncAccess(context.Background())
	if err != nil || initial.Receipt == nil || initial.Replayed {
		t.Fatalf("initial sync: %+v %v", initial, err)
	}
	firstKey, firstBody := fake.lastSyncKey, fake.lastSyncBody
	if firstKey == "" {
		t.Fatal("initial key missing")
	}

	forced, err := coordinator.SyncAccessForced(context.Background())
	if err != nil || forced.Receipt == nil || forced.Replayed {
		t.Fatalf("forced sync: %+v %v", forced, err)
	}
	if fake.syncCalls != 2 || fake.lastSyncKey == firstKey {
		t.Fatalf("forced sync must POST a new key: calls=%d key=%q first=%q", fake.syncCalls, fake.lastSyncKey, firstKey)
	}
	if fake.lastSyncBody != firstBody {
		t.Fatalf("forced sync must keep the same admissible body: %q != %q", firstBody, fake.lastSyncBody)
	}
	if store.Value == nil || store.Value.Key != fake.lastSyncKey {
		t.Fatalf("forced key must be durable before the POST: %+v", store.Value)
	}

	// A plain SyncAccess must now replay the forced terminal operation unchanged.
	replay, err := coordinator.SyncAccess(context.Background())
	if err != nil || replay.Receipt == nil || !replay.Replayed || fake.syncCalls != 2 {
		t.Fatalf("plain sync must replay the forced operation: %+v calls=%d err=%v", replay, fake.syncCalls, err)
	}
}

func TestSyncAccessForcedReplaysPendingWithoutSecondKey(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("7", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.syncResponse = syncBody("op-1", "pending")
	forced, err := coordinator.SyncAccessForced(context.Background())
	if err != nil || forced.Receipt == nil || forced.Receipt.Response == nil || forced.Receipt.Response.AccessApplicationState != "pending" {
		t.Fatalf("forced pending: %+v %v", forced, err)
	}
	key := fake.lastSyncKey
	again, err := coordinator.SyncAccessForced(context.Background())
	if err != nil || again.Receipt == nil {
		t.Fatalf("second forced call: %+v %v", again, err)
	}
	if fake.syncCalls != 1 || fake.lastSyncKey != key {
		t.Fatalf("pending renewal must not mint a second key: calls=%d key=%q", fake.syncCalls, fake.lastSyncKey)
	}
	if !again.Replayed {
		t.Fatalf("pending forced call must replay: %+v", again)
	}
}

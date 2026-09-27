package main

// S5-074: after a same-revision GET /gateways refresh is applied, the proven-deadline
// timer must re-arm on the refreshed catalog and the superseded deadline must never stop
// the data plane. mobileDeadlineNow/newDeadlineTimer are the package seams that let these
// tests drive the stop deterministically instead of sleeping on the wall clock.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"wg-turn-client/accountaccess"
)

// deadlineFixFixture is a fake /me + /gateways backend whose catalog body is replaced
// between refreshes.
type deadlineFixFixture struct {
	mu      sync.Mutex
	me      string
	catalog string
}

func (f *deadlineFixFixture) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		f.mu.Lock()
		body := ""
		switch request.URL.Path {
		case "/me":
			body = f.me
		case "/gateways":
			body = f.catalog
		default:
			f.mu.Unlock()
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		f.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, body)
	})
}

func (f *deadlineFixFixture) setCatalog(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.catalog = body
}

func deadlineFixMeBody(deadline string) string {
	return fmt.Sprintf(`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-26T13:00:00Z",
		"schema_version":"1.0","status":"ok","account_state":"ACTIVE_TRIAL","telegram_linked":true,
		"entitlement":{"type":"trial","status":"active","valid_from":"2026-09-26T09:00:00Z",
			"valid_until":"2026-09-30T09:00:00Z","effective_device_limit":2,"slots_used":1,
			"revision":"3","perpetual_commercial":false},
		"binding_status":"active","binding_revision":"1","management_only":false,
		"onboarding":{"state":"not_started","started_by":"server_confirmed_first_connection",
			"started_at":null,"not_after":null,"duration_seconds":3600,"one_time":true,
			"extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
			"requires_hardware_id":false,"unit":"installation_fingerprint",
			"post_telegram_identity":"account_history_correlation",
			"pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
		"grant_resolution":{"control_available":true,"restricted_checkout_available":true,
			"data_access":"onboarding_hour","effective_deadline":%q},
		"revision":"7"}`, deadline)
}

func deadlineFixCatalogBody(revision, issuedAt, validUntil, leaseNotAfter string) string {
	return fmt.Sprintf(`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-26T13:00:00Z",
		"schema_version":"1.0","status":"ok","revision":%q,"valid_until":%q,"issued_at":%q,
		"gateways":[{"gateway_id":"gw-0","name":"Synthetic 0","region":"test","country_code":"XX",
			"capabilities":["managed"],"target_workers":36,
			"transport":{"protocol":"wdtt-v17","peer_ip":"127.0.0.1","dtls_port":56300,"wg_port":56302,
				"dtls_spki_sha256":"PAyFy4YlbMsIfA4GfHvX_r8-HUWKVuj9PUeJ4pEtAjo"},
			"access":{"grant_id":"grant-0","device_ref":"dev-0","password":"synthetic",
				"generation":"1","lease_seq":"1","not_after":%q}}]}`, revision, validUntil, issuedAt, leaseNotAfter)
}

type fakeDeadlineEntry struct {
	at    time.Time
	fire  func()
	timer *time.Timer
}

// fakeDeadlineSchedule is a deterministic scheduler: every armed timer records its
// absolute deadline and fires only when advance passes it AND it was not stopped by a
// later armDeadline/stop decision.
type fakeDeadlineSchedule struct {
	mu      sync.Mutex
	now     time.Time
	entries []*fakeDeadlineEntry
}

func (f *fakeDeadlineSchedule) current() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeDeadlineSchedule) setNow(now time.Time) {
	f.mu.Lock()
	f.now = now
	f.mu.Unlock()
}

func (f *fakeDeadlineSchedule) afterFunc(delay time.Duration, fire func()) *time.Timer {
	timer := time.NewTimer(time.Hour)
	f.mu.Lock()
	f.entries = append(f.entries, &fakeDeadlineEntry{at: f.now.Add(delay), fire: fire, timer: timer})
	f.mu.Unlock()
	return timer
}

func (f *fakeDeadlineSchedule) lastArmed() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		return time.Time{}
	}
	return f.entries[len(f.entries)-1].at
}

func (f *fakeDeadlineSchedule) advance(to time.Time) int {
	f.mu.Lock()
	entries := append([]*fakeDeadlineEntry(nil), f.entries...)
	f.mu.Unlock()
	fires := 0
	for _, entry := range entries {
		if entry.at.After(to) {
			continue
		}
		if entry.timer.Stop() {
			entry.fire()
			fires++
		}
	}
	return fires
}

func installFakeDeadlineSchedule(t *testing.T, now time.Time) *fakeDeadlineSchedule {
	t.Helper()
	schedule := &fakeDeadlineSchedule{now: now}
	previousNow, previousTimer := mobileDeadlineNow, newDeadlineTimer
	mobileDeadlineNow = schedule.current
	newDeadlineTimer = schedule.afterFunc
	t.Cleanup(func() {
		mobileDeadlineNow = previousNow
		newDeadlineTimer = previousTimer
	})
	return schedule
}

func silenceDeadlineFixDiag(t *testing.T) {
	t.Helper()
	previous := diagStageDiagnostics
	diagStageDiagnostics = io.Discard
	t.Cleanup(func() { diagStageDiagnostics = previous })
}

type deadlineFixHarness struct {
	fixture     *deadlineFixFixture
	coordinator *accountaccess.Coordinator
	me          accountaccess.MeResponse
	initial     accountaccess.CatalogResponse
	mobile      *managedMobile
	schedule    *fakeDeadlineSchedule
	stopped     int
}

func newDeadlineFixHarness(t *testing.T, now, meDeadline, issuedAt, validUntil, leaseNotAfter time.Time) *deadlineFixHarness {
	t.Helper()
	fixture := &deadlineFixFixture{
		me:      deadlineFixMeBody(feedStamp(meDeadline)),
		catalog: deadlineFixCatalogBody("90", feedStamp(issuedAt), feedStamp(validUntil), feedStamp(leaseNotAfter)),
	}
	server := httptest.NewServer(fixture.handler())
	t.Cleanup(server.Close)
	subject := accountaccess.Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, err := accountaccess.NewCoordinator(accountaccess.Options{
		Client:    &accountaccess.Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: accountaccess.StaticToken("test-bearer")},
		AttemptID: "attempt",
		Store:     &accountaccess.MemoryReceiptStore{},
		Subject:   func() accountaccess.Subject { return subject },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	me, err := accountaccess.DecodeMeStrict([]byte(fixture.me))
	if err != nil {
		t.Fatal(err)
	}
	initial, err := coordinator.RefreshCatalog(context.Background())
	if err != nil || !initial.Applied || initial.Catalog == nil {
		t.Fatalf("initial catalog must apply: %+v %v", initial, err)
	}
	harness := &deadlineFixHarness{
		fixture:     fixture,
		coordinator: coordinator,
		me:          me,
		initial:     *initial.Catalog,
		schedule:    installFakeDeadlineSchedule(t, now),
	}
	controller := &managedController{}
	t.Cleanup(controller.registerMobileStop(func() { harness.stopped++ }, &managedDiagnostics{}))
	harness.mobile = &managedMobile{controller: controller, fingerprint: "fp"}
	harness.mobile.me = &harness.me
	harness.mobile.catalog = &harness.initial
	return harness
}

// admit feeds one catalog through the same DecideAdmission -> handleAdmission path the
// runner uses at accountaccess/runner.go:232-234.
func (h *deadlineFixHarness) admit(t *testing.T, catalog *accountaccess.CatalogResponse, now time.Time) accountaccess.AdmissionDecision {
	t.Helper()
	copied := *catalog
	h.mobile.me = &h.me
	h.mobile.catalog = &copied
	decision := accountaccess.DecideAdmission(h.me, copied, "gw-0", now)
	if !decision.Admitted {
		t.Fatalf("catalog must admit gw-0: %+v", decision)
	}
	h.mobile.handleAdmission(decision)
	return decision
}

func TestMobileSameRevisionRefreshRearmsProvenDeadline(t *testing.T) {
	silenceDeadlineFixDiag(t)
	issuedV1 := time.Date(2026, 9, 26, 13, 11, 7, 574_000_000, time.UTC)
	validV1 := issuedV1.Add(10 * time.Minute)
	issuedV2 := time.Date(2026, 9, 26, 13, 21, 6, 237_000_000, time.UTC)
	validV2 := time.Date(2026, 9, 26, 13, 21, 26, 938_000_000, time.UTC)
	harness := newDeadlineFixHarness(t, issuedV1, issuedV1.Add(time.Hour), issuedV1, validV1, validV1)

	harness.admit(t, &harness.initial, issuedV1)
	if armed := harness.schedule.lastArmed(); !armed.Equal(validV1) {
		t.Fatalf("initial proven deadline armed %v, want %v", armed, validV1)
	}
	if fires := harness.schedule.advance(validV1.Add(-time.Nanosecond)); fires != 0 || harness.stopped != 0 {
		t.Fatalf("advance before the deadline must not stop: fires=%d stopped=%d", fires, harness.stopped)
	}

	harness.fixture.setCatalog(deadlineFixCatalogBody("90", feedStamp(issuedV2), feedStamp(validV2), feedStamp(validV2)))
	harness.schedule.setNow(issuedV2)
	refreshed, err := harness.coordinator.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.State != accountaccess.StateApplied || !refreshed.Applied || refreshed.Catalog == nil {
		t.Fatalf("same-revision refresh must apply: %+v", refreshed)
	}
	if refreshed.Catalog.ValidUntil != feedStamp(validV2) || refreshed.Catalog.IssuedAt != feedStamp(issuedV2) {
		t.Fatalf("refreshed snapshot wrong: %+v", refreshed.Catalog)
	}

	harness.admit(t, refreshed.Catalog, issuedV2)
	if armed := harness.schedule.lastArmed(); !armed.Equal(validV2) {
		t.Fatalf("refreshed proven deadline armed %v, want %v", armed, validV2)
	}
	if fires := harness.schedule.advance(validV1); fires != 0 || harness.stopped != 0 {
		t.Fatalf("superseded deadline must not stop after refresh: fires=%d stopped=%d", fires, harness.stopped)
	}
	if fires := harness.schedule.advance(validV2.Add(-time.Nanosecond)); fires != 0 || harness.stopped != 0 {
		t.Fatalf("advance before the refreshed deadline must not stop: fires=%d stopped=%d", fires, harness.stopped)
	}
	if fires := harness.schedule.advance(validV2); fires != 1 || harness.stopped != 1 {
		t.Fatalf("refreshed deadline must stop exactly once: fires=%d stopped=%d", fires, harness.stopped)
	}
}

func TestMobileProvenDeadlineStopsWithoutRefresh(t *testing.T) {
	silenceDeadlineFixDiag(t)
	issuedV1 := time.Date(2026, 9, 26, 13, 11, 7, 574_000_000, time.UTC)
	validV1 := issuedV1.Add(10 * time.Minute)
	harness := newDeadlineFixHarness(t, issuedV1, issuedV1.Add(time.Hour), issuedV1, validV1, validV1)

	harness.admit(t, &harness.initial, issuedV1)
	if fires := harness.schedule.advance(validV1); fires != 1 || harness.stopped != 1 {
		t.Fatalf("unrefreshed deadline must stop at its bound: fires=%d stopped=%d", fires, harness.stopped)
	}
}

func TestMobileShortenedVerifiedAccessStopsImmediately(t *testing.T) {
	silenceDeadlineFixDiag(t)
	issuedV1 := time.Date(2026, 9, 26, 13, 11, 7, 574_000_000, time.UTC)
	validV1 := issuedV1.Add(10 * time.Minute)
	harness := newDeadlineFixHarness(t, issuedV1, issuedV1.Add(time.Hour), issuedV1, validV1, validV1)

	harness.admit(t, &harness.initial, issuedV1)
	if armed := harness.schedule.lastArmed(); !armed.Equal(validV1) {
		t.Fatalf("initial proven deadline armed %v, want %v", armed, validV1)
	}

	arrival := validV1.Add(-2 * time.Second)
	expiredLease := arrival.Add(-time.Second)
	harness.fixture.setCatalog(deadlineFixCatalogBody("90", feedStamp(arrival), feedStamp(validV1), feedStamp(expiredLease)))
	harness.schedule.setNow(arrival)
	refreshed, err := harness.coordinator.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.State != accountaccess.StateApplied || !refreshed.Applied || refreshed.Catalog == nil {
		t.Fatalf("shortened same-revision access must apply as-is: %+v", refreshed)
	}
	copied := *refreshed.Catalog
	harness.mobile.me = &harness.me
	harness.mobile.catalog = &copied
	decision := accountaccess.DecideAdmission(harness.me, copied, "gw-0", arrival)
	if !decision.StopDataPlane || decision.Reason != "GRANT_EXPIRED" {
		t.Fatalf("shortened access must stop fail-closed: %+v", decision)
	}
	harness.mobile.handleAdmission(decision)
	if harness.stopped != 1 {
		t.Fatalf("shortened/expired access must stop immediately: stopped=%d", harness.stopped)
	}
	if fires := harness.schedule.advance(validV1); fires != 0 || harness.stopped != 1 {
		t.Fatalf("stopped deadline must not fire again: fires=%d stopped=%d", fires, harness.stopped)
	}
}

func TestMobileOlderLateSameRevisionCannotRearm(t *testing.T) {
	silenceDeadlineFixDiag(t)
	issuedV1 := time.Date(2026, 9, 26, 13, 11, 7, 574_000_000, time.UTC)
	validV1 := issuedV1.Add(10 * time.Minute)
	issuedV0 := issuedV1.Add(-time.Minute)
	validV3 := validV1.Add(30 * time.Minute)
	harness := newDeadlineFixHarness(t, issuedV1, issuedV1.Add(time.Hour), issuedV1, validV1, validV1)

	harness.admit(t, &harness.initial, issuedV1)
	arrival := validV1.Add(-2 * time.Second)
	harness.fixture.setCatalog(deadlineFixCatalogBody("90", feedStamp(issuedV0), feedStamp(validV3), feedStamp(validV3)))
	harness.schedule.setNow(arrival)
	late, err := harness.coordinator.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if late.State != accountaccess.StateStale || late.Applied || late.Catalog == nil {
		t.Fatalf("older late response must stay stale: %+v", late)
	}
	if late.Catalog.ValidUntil != feedStamp(validV1) {
		t.Fatalf("older response rolled back the retained catalog: %+v", late.Catalog)
	}
	harness.admit(t, late.Catalog, arrival)
	if armed := harness.schedule.lastArmed(); !armed.Equal(validV1) {
		t.Fatalf("older late response re-armed %v, want retained %v", armed, validV1)
	}
}

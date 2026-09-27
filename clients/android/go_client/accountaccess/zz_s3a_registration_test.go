package accountaccess

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func withRegistration(t *testing.T, me string, registration map[string]any) string {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(me), &object); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	object["registration"] = registration
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestMeRegistrationBlockStrict(t *testing.T) {
	registered := withRegistration(t, hourMeActive, map[string]any{
		"state": "registered", "telegram_id": 42, "registered_at": "2026-09-23T19:55:00Z",
		"within_hour": true, "trial_available": true,
		"trial_reason": "within_hour_no_prior_trial", "purchase_available": true,
	})
	me, err := DecodeMeStrict([]byte(registered))
	if err != nil {
		t.Fatalf("registered decode: %v", err)
	}
	if me.Registration.State != "registered" || !me.Registration.TrialAvailable ||
		me.Registration.TrialReason == nil || *me.Registration.TrialReason != "within_hour_no_prior_trial" {
		t.Fatalf("registration projection wrong: %+v", me.Registration)
	}
	if me.Registration.TelegramID == nil || *me.Registration.TelegramID != 42 {
		t.Fatalf("telegram id lost: %+v", me.Registration)
	}

	// An absent block is the pre-registration server and stays valid.
	if _, err := DecodeMeStrict([]byte(hourMeExpired)); err != nil {
		t.Fatalf("absent registration must stay compatible: %v", err)
	}

	// Unknown state and unknown reason are rejected, never silently normalized.
	if _, err := DecodeMeStrict([]byte(withRegistration(t, hourMeActive, map[string]any{
		"state": "weird", "within_hour": false, "trial_available": false, "trial_reason": nil,
		"purchase_available": true}))); err == nil {
		t.Fatal("unknown registration state must be rejected")
	}
	if _, err := DecodeMeStrict([]byte(withRegistration(t, hourMeActive, map[string]any{
		"state": "registered", "within_hour": false, "trial_available": false,
		"trial_reason": "made_up", "purchase_available": true}))); err == nil {
		t.Fatal("unknown trial_reason must be rejected")
	}
}

func TestRegistrationLinkStrict(t *testing.T) {
	link, err := DecodeRegistrationLinkStrict([]byte(`{"request_id":"0123456789abcdef0123456789abcdef",` +
		`"server_time":"2026-09-23T20:00:00Z","schema_version":"1.0","status":"ok","registration":{"state":"pending","token":"tok123",` +
		`"bot_username":"terlimo_bot","deep_link":"https://t.me/terlimo_bot?start=tok123",` +
		`"expires_at":"2026-09-23T20:10:00Z","expires_in":600}}`))
	if err != nil {
		t.Fatalf("pending link: %v", err)
	}
	if link.State != "pending" || link.DeepLink != "https://t.me/terlimo_bot?start=tok123" || link.Token != "tok123" {
		t.Fatalf("link projection wrong: %+v", link)
	}

	if _, err := DecodeRegistrationLinkStrict([]byte(`{"request_id":"0123456789abcdef0123456789abcdef",` +
		`"server_time":"2026-09-23T20:00:00Z","schema_version":"1.0","status":"ok","registration":{"state":"pending","token":"tok123",` +
		`"bot_username":"terlimo_bot","deep_link":"https://evil.example/tok123",` +
		`"expires_at":"2026-09-23T20:10:00Z","expires_in":600}}`)); err == nil {
		t.Fatal("mismatched deep link must be rejected")
	}
	registered, err := DecodeRegistrationLinkStrict([]byte(`{"request_id":"0123456789abcdef0123456789abcdef",` +
		`"server_time":"2026-09-23T20:00:00Z","schema_version":"1.0","status":"ok","registration":{"state":"registered"}}`))
	if err != nil || registered.State != "registered" {
		t.Fatalf("registered link: %+v %v", registered, err)
	}
}

func TestRequestRegistrationLinkClient(t *testing.T) {
	var seenAuth, seenBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		body := make([]byte, 64)
		n, _ := r.Body.Read(body)
		seenBody = string(body[:n])
		if r.Method != http.MethodPost || r.URL.Path != "/api/mobile/v1/registration/telegram/link" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-23T20:00:00Z",` +
			`"schema_version":"1.0","status":"ok",` +
			`"registration":{"state":"pending","token":"tok","bot_username":"terlimo_bot",` +
			`"deep_link":"https://t.me/terlimo_bot?start=tok","expires_at":"2026-09-23T20:10:00Z","expires_in":600}}`))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: StaticToken("bearer-1")}
	link, apiError, err := client.RequestRegistrationLink(context.Background())
	if err != nil || apiError != nil || link.State != "pending" {
		t.Fatalf("link request: %+v %v %v", link, apiError, err)
	}
	if seenAuth != "Bearer bearer-1" {
		t.Fatalf("session bearer must authorize the request: %q", seenAuth)
	}
	if seenBody != "{}" {
		t.Fatalf("body must be empty JSON: %q", seenBody)
	}
}

func TestProjectionCarriesRegistrationDisplaySubset(t *testing.T) {
	reason := "hour_expired"
	projection := buildProjection(MeResponse{
		Registration: meRegistration{State: "registered", WithinHour: false,
			TrialAvailable: false, TrialReason: &reason, PurchaseAvailable: true},
	}, 1, nil)
	payload := projection.Payload()
	registration, ok := payload["registration"].(map[string]any)
	if !ok {
		t.Fatalf("registration missing from payload: %v", payload["registration"])
	}
	if registration["state"] != "registered" || registration["purchase_available"] != true ||
		registration["trial_reason"] != "hour_expired" {
		t.Fatalf("registration payload wrong: %v", registration)
	}
	if _, leaked := registration["telegram_id"]; leaked {
		t.Fatal("telegram_id must not reach the host projection")
	}
}

func TestRegistrationLinkStrictRejectsFlatAndMalformed(t *testing.T) {
	// Flat legacy shape (no envelope) must be rejected, never normalized.
	flat := `{"state":"pending","token":"tok","bot_username":"terlimo_bot",` +
		`"deep_link":"https://t.me/terlimo_bot?start=tok","expires_at":"2026-09-23T20:10:00Z","expires_in":600}`
	if _, err := DecodeRegistrationLinkStrict([]byte(flat)); err == nil {
		t.Fatal("flat shape must be rejected")
	}
	// Missing server_time (now required per schemas/common.json) rejected.
	if _, err := DecodeRegistrationLinkStrict([]byte(`{"request_id":"0123456789abcdef0123456789abcdef",` +
		`"schema_version":"1.0","status":"ok","registration":{"state":"registered"}}`)); err == nil {
		t.Fatal("missing server_time must be rejected")
	}
	// Invalid server_time rejected.
	if _, err := DecodeRegistrationLinkStrict([]byte(`{"request_id":"0123456789abcdef0123456789abcdef",` +
		`"server_time":"not-a-time","schema_version":"1.0","status":"ok","registration":{"state":"registered"}}`)); err == nil {
		t.Fatal("invalid server_time must be rejected")
	}
	// Unknown additive field rejected (strict).
	if _, err := DecodeRegistrationLinkStrict([]byte(`{"request_id":"0123456789abcdef0123456789abcdef",` +
		`"server_time":"2026-09-23T20:00:00Z","schema_version":"1.0","status":"ok","extra":1,` +
		`"registration":{"state":"registered"}}`)); err == nil {
		t.Fatal("unknown top-level field must be rejected")
	}
	// Pending with a malformed expiry rejected.
	if _, err := DecodeRegistrationLinkStrict([]byte(`{"request_id":"0123456789abcdef0123456789abcdef",` +
		`"server_time":"2026-09-23T20:00:00Z","schema_version":"1.0","status":"ok",` +
		`"registration":{"state":"pending","token":"tok","bot_username":"terlimo_bot",` +
		`"deep_link":"https://t.me/terlimo_bot?start=tok","expires_at":"soon","expires_in":600}}`)); err == nil {
		t.Fatal("malformed pending expiry must be rejected")
	}
}

func TestMeRegistrationPendingLinkExpiresAtDecodesAndReachesCatalog(t *testing.T) {
	// Pending link adds the additive `link_expires_at` key; strict decode must accept it.
	pending := withRegistration(t, hourMeActive, map[string]any{
		"state": "pending", "telegram_id": nil, "registered_at": nil, "within_hour": true,
		"trial_available": true, "trial_reason": "within_hour_no_prior_trial",
		"purchase_available": true, "link_expires_at": "2026-09-24T17:10:00Z",
	})
	if _, err := DecodeMeStrict([]byte(pending)); err != nil {
		t.Fatalf("pending registration with link_expires_at must decode: %v", err)
	}
	// Malformed optional value is rejected.
	bad := withRegistration(t, hourMeActive, map[string]any{
		"state": "pending", "within_hour": true, "trial_available": true,
		"trial_reason": "within_hour_no_prior_trial", "purchase_available": true,
		"link_expires_at": "soon",
	})
	if _, err := DecodeMeStrict([]byte(bad)); err == nil {
		t.Fatal("malformed link_expires_at must be rejected")
	}

	// Regression: a /me with registration pending + link_expires_at must let the runner
	// cycle reach GET /gateways (previously decode failed before the catalog call).
	fixture := newAuthFixture(t)
	fixture.installations[InstallationFingerprint(fixture.spkiDER)] = true
	fixture.linked = false // unlinked scopes: no access:sync, cycle returns at catalog
	auth := fixture.handler()
	var gateways int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/mobile/v1/me":
			_, _ = w.Write([]byte(pending))
		case "/api/mobile/v1/gateways":
			gateways++
			_, _ = w.Write([]byte(catalogBody("4", 1)))
		default:
			auth.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	session := newFixtureSession(t, fixture, server)
	coordinator, err := NewCoordinator(Options{
		Client:    &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session},
		AttemptID: "attempt", Store: &MemoryReceiptStore{}, Subject: session.Subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{
		Session: session, Coordinator: coordinator, Attempts: 3, RefreshFloor: time.Minute,
		Jitter: func(time.Duration) time.Duration { return 0 },
		Emit:   func(context.Context, map[string]any) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog, _, err := runner.cycle(context.Background())
	if err != nil || catalog == nil {
		t.Fatalf("cycle after /me with link_expires_at: catalog=%v err=%v", catalog, err)
	}
	if gateways != 1 {
		t.Fatalf("GET /gateways must be reached exactly once, got %d", gateways)
	}
}

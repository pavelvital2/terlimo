package accountaccess

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// STEP036 hour wire fixtures (synthetic copies of the accepted backend bundle).

const hourMeActive = `{"account_state": "UNLINKED", "binding_revision": "1", "binding_status": "none", "entitlement": {"effective_device_limit": 2, "perpetual_commercial": false, "revision": "0", "slots_used": 0, "status": "none", "type": "none", "valid_from": null, "valid_until": null}, "grant_resolution": {"control_available": true, "data_access": "onboarding_hour", "effective_deadline": "2026-09-23T20:51:47Z", "restricted_checkout_available": true}, "management_only": false, "onboarding": {"creates_trial": false, "duration_seconds": 3600, "extends_on_refresh": false, "extends_on_restart": false, "not_after": "2026-09-23T20:51:47Z", "one_time": true, "post_telegram_identity": "account_history_correlation", "pre_telegram_reinstall": "may_be_indistinguishable_new_key_separate_unit", "requires_hardware_id": false, "revision": 1, "started_at": "2026-09-23T19:51:47Z", "started_by": "server_confirmed_first_connection", "state": "active", "unit": "installation_fingerprint"}, "request_id": "f2c6c6564a0518ed6ebe056ba8389199", "revision": "1", "schema_version": "1.0", "server_time": "2026-09-23T19:51:47Z", "status": "ok", "telegram_linked": false}`
const hourMeExpired = `{"account_state": "UNLINKED", "binding_revision": null, "binding_status": "none", "entitlement": {"effective_device_limit": 2, "perpetual_commercial": false, "revision": "0", "slots_used": 0, "status": "none", "type": "none", "valid_from": null, "valid_until": null}, "grant_resolution": {"control_available": true, "data_access": "none", "effective_deadline": null, "restricted_checkout_available": true}, "management_only": false, "onboarding": {"creates_trial": false, "duration_seconds": 3600, "extends_on_refresh": false, "extends_on_restart": false, "not_after": "2026-09-23T20:51:47Z", "one_time": true, "post_telegram_identity": "account_history_correlation", "pre_telegram_reinstall": "may_be_indistinguishable_new_key_separate_unit", "requires_hardware_id": false, "revision": 2, "started_at": "2026-09-23T19:51:47Z", "started_by": "server_confirmed_first_connection", "state": "expired", "unit": "installation_fingerprint"}, "request_id": "9e3eb6fb9f92b0757dc8f5351279e4ae", "revision": "2", "schema_version": "1.0", "server_time": "2026-09-23T19:51:47Z", "status": "ok", "telegram_linked": false}`
const hourGatewaysData = `{"gateways": [{"access": {"device_ref": "961708eb1216df730fc4187e7c2c00a94d2cbcaffc3fe8ffff74ec9c4e4701d1", "generation": "1", "grant_id": "369bb876-2547-4798-8cdf-2bb5e99aad7c", "lease_seq": "1", "not_after": "2026-09-23T20:06:47Z", "password": "CUCQNlmws0AEODrUXmG2HizCtlNbOTs6"}, "capabilities": ["managed"], "gateway_id": "terlimo-035-node", "name": "terlimo-035-node", "target_workers": 36, "transport": {"dtls_port": 57400, "dtls_spki_sha256": "cHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHA", "peer_ip": "127.0.0.1", "protocol": "wdtt-v17", "wg_port": 57401}}], "issued_at": "2026-09-23T19:51:47Z", "request_id": "cd6b67d57dc1b89cb0af48d1b3e202c8", "revision": "3", "schema_version": "1.0", "server_time": "2026-09-23T19:51:47Z", "status": "ok", "valid_until": "2026-09-23T20:01:47Z"}`
const hourGatewaysExpired = `{"catalog_mode": "browse", "gateways": [{"gateway_id": "terlimo-035-node", "name": "terlimo-035-node"}], "issued_at": "2026-09-23T19:51:47Z", "request_id": "f2547520141a65ed584d5ec45c8fdf45", "schema_version": "1.0", "server_time": "2026-09-23T19:51:47Z", "status": "ok", "valid_until": "2026-09-23T20:01:47Z"}`
const hourGatewaysPending = `{"code": "ACCESS_SYNC_PENDING", "details": {"binding_revision": "1", "catalog_revision": "1"}, "request_id": "2677c112d0e520e7549e807f534f1829", "retry_after_ms": 1000, "retryable": true, "schema_version": "1.0", "server_time": "2026-09-23T19:51:47Z", "status": "error"}`


// Real accepted shapes must decode with the existing strict parsers and map to the
// hour semantics (numeric binding_revision slot, management_only incompatible).
func TestHourFixturesDecodeWithExistingParsers(t *testing.T) {
	active, err := DecodeMeStrict([]byte(hourMeActive))
	if err != nil {
		t.Fatalf("active hour me: %v", err)
	}
	if active.GrantResolution.DataAccess != "onboarding_hour" || active.ManagementOnly ||
		active.Onboarding.State != "active" || active.GrantResolution.EffectiveDeadline == nil {
		t.Fatalf("active hour shape wrong: %+v", active.GrantResolution)
	}
	// Fixture relation: the active hour binding_revision is the onboarding revision
	// (the entitlement revision is an independent counter and may differ).
	if active.BindingRevision == nil || *active.BindingRevision != active.Onboarding.Revision.String() {
		t.Fatalf("hour binding_revision must be the onboarding revision: %v vs %v", active.BindingRevision, active.Onboarding.Revision)
	}

	expired, err := DecodeMeStrict([]byte(hourMeExpired))
	if err != nil {
		t.Fatalf("expired hour me: %v", err)
	}
	if expired.GrantResolution.DataAccess != "none" || expired.BindingRevision != nil ||
		expired.GrantResolution.EffectiveDeadline != nil || expired.Onboarding.State != "expired" {
		t.Fatalf("expired hour shape wrong: %+v", expired)
	}

	data, err := DecodeGatewaysStrict([]byte(hourGatewaysData))
	if err != nil || data.Catalog == nil || data.Browse != nil || len(data.Catalog.Gateways) == 0 {
		t.Fatalf("credential catalog shape: %+v err=%v", data, err)
	}
	browse, err := DecodeGatewaysStrict([]byte(hourGatewaysExpired))
	if err != nil || browse.Browse == nil || browse.Browse.CatalogMode != CatalogModeBrowse {
		t.Fatalf("revoked browse shape: %+v err=%v", browse, err)
	}
	pending, err := DecodeErrorStrict([]byte(hourGatewaysPending))
	if err != nil || pending.Code != CodeAccessSyncPending {
		t.Fatalf("pending envelope: %+v err=%v", pending, err)
	}
	if _, _, ok := pending.AdmissionTokens(); !ok {
		t.Fatal("pending must carry admission tokens")
	}
}

// Production path: the real Runner + MobileSession + Coordinator drive
// pending→sync→data without the test calling RefreshCatalog by hand; OnVerified
// fires in the same cycle sequence (no one-minute floor wait).
func TestHourPendingThenSyncThenDataThroughRunner(t *testing.T) {
	var mu sync.Mutex
	gatewayCalls, syncCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/mobile/v1/auth/challenge":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2030-01-01T00:00:00Z",
				"schema_version": "1.0", "status": "ok", "challenge_id": "00112233445566778899aabbccddeeff",
				"nonce_b64": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "purpose": "session",
				"environment": "test", "expires_at": "2030-01-01T00:05:00Z", "single_use": true})
		case r.URL.Path == "/api/mobile/v1/auth/session":
			// Installation hour: the account stays unlinked (account_ref null) but the
			// preferred data scopes are granted for this installation.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2030-01-01T00:00:00Z",
				"schema_version": "1.0", "status": "ok",
				"session": map[string]any{"session_id": "tok-hour", "account_ref": nil,
					"installation_ref": "inst", "scopes": []string{"session:read", "access:sync"}, "generation": "1",
					"issued_at": "2030-01-01T00:00:00Z", "expires_at": "2030-01-01T01:00:00Z"}})
		case r.URL.Path == "/api/mobile/v1/me":
			_, _ = io.WriteString(w, hourMeActive)
		case r.URL.Path == "/api/mobile/v1/gateways":
			mu.Lock(); gatewayCalls++; call := gatewayCalls; mu.Unlock()
			if call == 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, hourGatewaysPending)
				return
			}
			_, _ = io.WriteString(w, hourGatewaysData)
		case r.URL.Path == "/api/mobile/v1/access/sync":
			mu.Lock(); syncCalls++; mu.Unlock()
			_, _ = io.WriteString(w, syncBody("op-hour-runner", "applied"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewMobileSession(MobileConfig{
		BaseURL: server.URL, Environment: EnvironmentTest, SPKIDER: spki, HTTP: server.Client(),
		Signer: func(_ context.Context, transcript []byte) ([]byte, error) {
			d := sha256.Sum256(transcript)
			return ecdsa.SignASN1(rand.Reader, key, d[:])
		},
		Now: func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(Options{
		Client: &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session},
		AttemptID: "attempt", Store: &MemoryReceiptStore{}, Subject: session.Subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	verified := make(chan CatalogResponse, 1)
	runner, err := NewRunner(RunnerConfig{
		Session: session, Coordinator: coordinator,
		Emit: func(context.Context, map[string]any) error { return nil },
		OnVerified: func(_ MeResponse, catalog CatalogResponse) {
			select { case verified <- catalog: default: }
		},
		OnError: func(code string) { t.Logf("runner error code=%s", code) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	select {
	case catalog := <-verified:
		if len(catalog.Gateways) == 0 {
			t.Fatalf("verified catalog empty: %+v", catalog)
		}
	case <-time.After(5 * time.Second):
		mu.Lock(); g, s2 := gatewayCalls, syncCalls; mu.Unlock()
		t.Fatalf("runner did not reach verified: gateways=%d sync=%d scopes=%v", g, s2, session.Scopes())
	}
	mu.Lock(); g, s2 := gatewayCalls, syncCalls; mu.Unlock()
	if g < 2 || s2 != 1 {
		t.Fatalf("sequence wrong: gateways=%d sync=%d", g, s2)
	}
}

// Revoked/expired hour data must stay display-only: browse is not data-ready.
func TestHourRevokedBrowseIsNotDataReady(t *testing.T) {
	var mu sync.Mutex
	gatewayCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me":
			_, _ = io.WriteString(w, hourMeExpired)
		case r.Method == http.MethodGet && r.URL.Path == "/gateways":
			mu.Lock(); gatewayCalls++; mu.Unlock()
			_, _ = io.WriteString(w, hourGatewaysExpired)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	coordinator, err := NewCoordinator(Options{
		Client: &Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: StaticToken("bearer")},
		AttemptID: "attempt", Store: &MemoryReceiptStore{},
		Subject: func() Subject { return Subject{AccountRef: "acc", InstallationID: "inst"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh expired: %v", err)
	}
	res, err := coordinator.RefreshCatalog(context.Background())
	if err != nil || res.Browse == nil || res.Admission != nil || res.Applied {
		t.Fatalf("revoked hour must be browse-only, never data-ready: %+v err=%v", res, err)
	}
	_ = json.Valid([]byte(hourGatewaysPending))
	_ = strings.TrimSpace("")
}

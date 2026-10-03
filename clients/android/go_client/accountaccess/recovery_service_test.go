package accountaccess

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Reuse the real challenge/proof/session fixture, while counting every HTTP route.
// The recovery seam must not silently expand into enrollment or admission work.
type recoveryServiceHTTP struct {
	auth           *authFixture
	mu             sync.Mutex
	calls          map[string]int
	me             string
	foreignSession bool
}

func (f *recoveryServiceHTTP) handler(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	f.calls[req.Method+" "+req.URL.Path]++
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if req.URL.Path == "/api/mobile/v1/me" {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		f.auth.mu.Lock()
		_, authenticated := f.auth.sessions[token]
		f.auth.mu.Unlock()
		if token == "" || !authenticated {
			f.auth.errorResponse(w, http.StatusUnauthorized, "SESSION_INVALID", "")
			return
		}
		_, _ = io.WriteString(w, f.me)
		return
	}
	if f.foreignSession && req.URL.Path == "/api/mobile/v1/auth/session" {
		recorded := httptest.NewRecorder()
		f.auth.handler().ServeHTTP(recorded, req)
		if recorded.Code != http.StatusOK {
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
			return
		}
		var envelope map[string]any
		_ = json.Unmarshal(recorded.Body.Bytes(), &envelope)
		envelope["session"].(map[string]any)["installation_ref"] = strings.Repeat("f", 64)
		_ = json.NewEncoder(w).Encode(envelope)
		return
	}
	f.auth.handler().ServeHTTP(w, req)
}

func (f *recoveryServiceHTTP) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method+" /api/mobile/v1"+path]
}

type recoveryServiceReceipt struct {
	value *Receipt
	saves int
}

func (s *recoveryServiceReceipt) Load() (*Receipt, error)  { return s.value, nil }
func (s *recoveryServiceReceipt) Save(value Receipt) error { s.saves++; s.value = &value; return nil }

type recoveryServiceDoer struct {
	base    Doer
	afterMe func()
}

func (d recoveryServiceDoer) Do(req *http.Request) (*http.Response, error) {
	response, err := d.base.Do(req)
	if err == nil && req.URL.Path == "/api/mobile/v1/me" && d.afterMe != nil {
		d.afterMe()
	}
	return response, err
}

func recoveryServiceFixture(t *testing.T, linked, enrolled bool) (*recoveryServiceHTTP, *Runner, *MobileSession, *recoveryServiceReceipt) {
	t.Helper()
	auth := newAuthFixture(t)
	auth.linked = linked
	if enrolled {
		auth.installations[InstallationFingerprint(auth.spkiDER)] = true
	}
	body := meBody("7", nil, "not_started")
	body = strings.Replace(body, `"account_state":"ACTIVE_TRIAL"`, `"account_state":"UNLINKED"`, 1)
	body = strings.Replace(body, `"binding_status":"active"`, `"binding_status":"none"`, 1)
	body = strings.Replace(body, `"telegram_linked":true`, `"telegram_linked":false`, 1)
	body = strings.Replace(body, `"management_only":false`, `"management_only":true`, 1)
	if linked {
		body = strings.TrimSuffix(body, "}") + `,"account_ref":"acc-fixture-1"}`
	}
	fixture := &recoveryServiceHTTP{auth: auth, calls: map[string]int{}, me: body}
	server := httptest.NewServer(http.HandlerFunc(fixture.handler))
	t.Cleanup(server.Close)
	session := newFixtureSession(t, auth, server)
	session.config.DisableEnrollment = true
	store := &recoveryServiceReceipt{value: &Receipt{Key: "retained-access-receipt"}}
	coordinator, err := NewCoordinator(Options{
		Client:    &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session},
		AttemptID: "recovery-test-attempt", Store: store, Subject: session.Subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{
		Session: session, Coordinator: coordinator,
		Emit: func(context.Context, map[string]any) error {
			t.Error("recovery emitted account/admission projection")
			return nil
		},
		OnVerified: func(MeResponse, CatalogResponse) { t.Error("recovery established a catalog/paid readiness") },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture, runner, session, store
}

func recoveryServiceAssertReadOnly(t *testing.T, fixture *recoveryServiceHTTP, store *recoveryServiceReceipt) {
	t.Helper()
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/gateways"}, {http.MethodPost, "/access/sync"},
		{http.MethodPost, "/installations"}, {http.MethodPost, "/payments"},
		{http.MethodPost, "/quotes"}, {http.MethodPost, "/onboarding/intents"},
	} {
		if count := fixture.count(route.method, route.path); count != 0 {
			t.Errorf("unexpected %s %s calls: %d", route.method, route.path, count)
		}
	}
	if store.saves != 0 || store.value == nil || store.value.Key != "retained-access-receipt" {
		t.Fatalf("recovery changed durable receipt: saves=%d value=%+v", store.saves, store.value)
	}
}

func TestRecoveryExistingInstallationAuthenticatesManagementWithoutCatalog(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "management-only"
		if linked {
			name = "linked"
		}
		t.Run(name, func(t *testing.T) {
			fixture, runner, session, store := recoveryServiceFixture(t, linked, true)
			fingerprint := session.Fingerprint()
			if err := runner.AuthenticateServiceOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if fixture.count(http.MethodGet, "/me") != 1 {
				t.Fatal("recovery must read exactly one authenticated /me")
			}
			if session.Fingerprint() != fingerprint {
				t.Fatal("recovery changed installation identity")
			}
			if !linked && !contains(session.Scopes(), "management-only") {
				t.Fatalf("management-only session missing: %v", session.Scopes())
			}
			fixture.auth.mu.Lock()
			proofs := fixture.auth.proofsVerified
			fixture.auth.mu.Unlock()
			if proofs == 0 {
				t.Fatal("recovery bypassed PoP authentication")
			}
			recoveryServiceAssertReadOnly(t, fixture, store)
		})
	}
}

func TestRecoveryUnknownInstallationDoesNotEnroll(t *testing.T) {
	fixture, runner, _, store := recoveryServiceFixture(t, false, false)
	if err := runner.AuthenticateServiceOnce(context.Background()); err == nil {
		t.Fatal("unknown installation became recovery-ready")
	}
	if fixture.count(http.MethodGet, "/me") != 0 {
		t.Fatal("/me called after rejected authentication")
	}
	recoveryServiceAssertReadOnly(t, fixture, store)
}

func TestRecoveryForeignInstallationSessionIsRejected(t *testing.T) {
	fixture, runner, _, store := recoveryServiceFixture(t, false, true)
	fixture.foreignSession = true
	if err := runner.AuthenticateServiceOnce(context.Background()); err == nil {
		t.Fatal("foreign installation_ref in auth/session became recovery-ready")
	}
	recoveryServiceAssertReadOnly(t, fixture, store)
}

func TestRecoveryCanceledForeignAccountAndReplacedSessionAreRejected(t *testing.T) {
	for _, mode := range []string{"already-canceled", "cancel-after-me", "foreign-account", "replaced-generation"} {
		t.Run(mode, func(t *testing.T) {
			fixture, runner, session, store := recoveryServiceFixture(t, false, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "already-canceled":
				cancel()
			case "cancel-after-me":
				runner.config.Coordinator.opts.Client.HTTP = recoveryServiceDoer{base: runner.config.Coordinator.opts.Client.HTTP, afterMe: cancel}
			case "foreign-account":
				fixture.me = strings.TrimSuffix(fixture.me, "}") + `,"account_ref":"foreign-account"}`
			case "replaced-generation":
				runner.config.Coordinator.opts.Client.HTTP = recoveryServiceDoer{base: runner.config.Coordinator.opts.Client.HTTP, afterMe: func() {
					session.mu.Lock()
					session.current.Generation = "2"
					session.mu.Unlock()
				}}
			}
			if err := runner.AuthenticateServiceOnce(ctx); err == nil {
				t.Fatal("canceled/foreign/replaced response became recovery-ready")
			}
			recoveryServiceAssertReadOnly(t, fixture, store)
		})
	}
}

func TestRecoveryServiceSeedExactAuthorizedGETAndUnavailable(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		name := "success"
		if unavailable {
			name = "unavailable"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				if req.Method != http.MethodGet || req.URL.Path != "/api/mobile/v1/service-seed" || req.URL.RawQuery != "" || req.Header.Get("Authorization") != "Bearer recovery-test-token" {
					t.Errorf("unexpected request: %s %s bearer=%t", req.Method, req.URL.String(), req.Header.Get("Authorization") != "")
				}
				raw, _ := io.ReadAll(req.Body)
				if len(raw) != 0 || req.Header.Get("Idempotency-Key") != "" {
					t.Error("service seed GET must have no body/idempotency key")
				}
				w.Header().Set("Content-Type", "application/json")
				if unavailable {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"SERVICE_UNAVAILABLE","retryable":true}`)
					return
				}
				_, _ = io.WriteString(w, `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"ok","recovery_code":"TR1.payload.signature"}`)
			}))
			defer server.Close()
			client := &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: StaticToken("recovery-test-token")}
			response, apiError, err := client.GetServiceSeed(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if unavailable {
				if apiError == nil || apiError.Code != "SERVICE_UNAVAILABLE" || response.RecoveryCode != "" {
					t.Fatalf("unavailable response fabricated success: %+v %+v", response, apiError)
				}
			} else if apiError != nil || response.RecoveryCode != "TR1.payload.signature" || response.ServerTime != "2026-10-03T00:00:00Z" {
				t.Fatalf("service seed response: %+v %+v", response, apiError)
			}
			if calls != 1 {
				t.Fatalf("service seed request retried: %d", calls)
			}
		})
	}
}

func TestRecoveryServiceSeedRejectsMalformedStrictEnvelope(t *testing.T) {
	valid := `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"ok","recovery_code":"TR1.payload.signature"}`
	cases := map[string]string{
		"missing-server-time":   strings.Replace(valid, `,"server_time":"2026-10-03T00:00:00Z"`, "", 1),
		"null-server-time":      strings.Replace(valid, `"2026-10-03T00:00:00Z"`, `null`, 1),
		"invalid-server-time":   strings.Replace(valid, "2026-10-03T00:00:00Z", "not-a-time", 1),
		"invalid-calendar-time": strings.Replace(valid, "2026-10-03T00:00:00Z", "2026-02-30T00:00:00Z", 1),
		"numeric-schema":        strings.Replace(valid, `"schema_version":"1.0"`, `"schema_version":1`, 1),
		"duplicate-server-time": strings.TrimSuffix(valid, "}") + `,"server_time":"2026-10-03T00:00:00Z"}`,
		"case-alias-extra-key":  strings.TrimSuffix(valid, "}") + `,"Server_Time":"2026-10-03T00:00:00Z"}`,
		"missing-code":          strings.Replace(valid, `,"recovery_code":"TR1.payload.signature"`, "", 1),
		"null-code":             strings.Replace(valid, `"TR1.payload.signature"`, `null`, 1),
		"empty-code":            strings.Replace(valid, `"TR1.payload.signature"`, `""`, 1),
		"oversized-code":        strings.Replace(valid, "TR1.payload.signature", strings.Repeat("x", 3501), 1),
		"foreign-schema":        strings.Replace(valid, `"1.0"`, `"2.0"`, 1),
		"invalid-request-id":    strings.Replace(valid, "0123456789abcdef0123456789abcdef", "invalid", 1),
		"unknown-field":         strings.TrimSuffix(valid, "}") + `,"extra":true}`,
		"duplicate-field":       strings.TrimSuffix(valid, "}") + `,"status":"ok"}`,
		"trailing-json":         valid + ` {}`,
		"trailing-invalid":      valid + ` trailing`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeServiceSeed([]byte(raw)); err == nil {
				t.Fatal("malformed service seed envelope accepted")
			}
		})
	}
}

func TestRecoveryServiceSeedTokenFailureHasNoAnonymousRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	client := &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: recoveryServiceFailedToken{}}
	if _, _, err := client.GetServiceSeed(context.Background()); err == nil {
		t.Fatal("token failure became success")
	}
	if calls != 0 {
		t.Fatalf("anonymous request after token failure: %d", calls)
	}
}

type recoveryServiceFailedToken struct{}

func (recoveryServiceFailedToken) Bearer(context.Context) (string, error) {
	return "", errors.New("test-token-unavailable")
}

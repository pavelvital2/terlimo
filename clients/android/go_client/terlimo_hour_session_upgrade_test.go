package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"wg-turn-client/accountaccess"
)

// After a successful hour activation the production refreshSessionAfterActivation
// must drop the management-only bearer so the next Ensure re-authenticates with the
// preferred access:sync scopes; the old management bearer is never upgraded in place.
func TestRefreshSessionAfterActivationUpgradesToPreferredScopes(t *testing.T) {
	var mu sync.Mutex
	sessionCalls := 0
	linked := false
	var requested [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/mobile/v1/auth/challenge":
			mu.Lock(); sessionCalls++; mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2030-01-01T00:00:00Z",
				"schema_version": "1.0", "status": "ok", "challenge_id": "00112233445566778899aabbccddeeff",
				"nonce_b64": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "purpose": "session",
				"environment": "test", "expires_at": "2030-01-01T00:05:00Z", "single_use": true})
		case "/api/mobile/v1/auth/session":
			mu.Lock(); sessionCalls++; lin := linked; mu.Unlock()
			var body struct {
				Proof struct {
					SignedPayloadB64 string `json:"signed_payload_b64"`
				} `json:"proof"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var payload struct {
				RequestedScopes []string `json:"requested_scopes"`
			}
			if raw, err := base64.RawURLEncoding.DecodeString(body.Proof.SignedPayloadB64); err == nil {
				_ = json.Unmarshal(raw, &payload)
			}
			mu.Lock(); requested = append(requested, append([]string(nil), payload.RequestedScopes...)); mu.Unlock()
			if containsAny(payload.RequestedScopes, "access:sync") && !lin {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2030-01-01T00:00:00Z",
					"schema_version": "1.0", "status": "error", "code": "ACCESS_DENIED", "retryable": false})
				return
			}
			// Installation hour: the account_ref stays null; scopes echo the request.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2030-01-01T00:00:00Z",
				"schema_version": "1.0", "status": "ok",
				"session": map[string]any{"session_id": "tok-" + time.Now().Format("150405.000000000"),
					"account_ref": nil, "installation_ref": "inst", "scopes": payload.RequestedScopes, "generation": "1",
					"issued_at": "2030-01-01T00:00:00Z", "expires_at": "2030-01-01T01:00:00Z"}})
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
	session, err := accountaccess.NewMobileSession(accountaccess.MobileConfig{
		BaseURL: server.URL, Environment: accountaccess.EnvironmentTest, SPKIDER: spki, HTTP: server.Client(),
		Signer: func(_ context.Context, transcript []byte) ([]byte, error) {
			digest := sha256.Sum256(transcript)
			return ecdsa.SignASN1(rand.Reader, key, digest[:])
		},
		Now: func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &managedMobile{session: session}
	before, err := session.Bearer(context.Background())
	if err != nil {
		t.Fatalf("pre-activation bearer: %v", err)
	}
	if scopes := session.Scopes(); !containsAny(scopes, "management-only") || containsAny(scopes, "access:sync") {
		t.Fatalf("pre-activation session must be the management fallback: %v", scopes)
	}

	mu.Lock(); linked = true; mu.Unlock() // the hour becomes active for this installation
	m.refreshSessionAfterActivation()

	after, err := session.Bearer(context.Background())
	if err != nil {
		t.Fatalf("post-activation bearer: %v", err)
	}
	if after == before {
		t.Fatal("old management bearer must not be reused after activation")
	}
	if scopes := session.Scopes(); containsAny(scopes, "management-only") || !containsAny(scopes, "access:sync") {
		t.Fatalf("post-activation session must be data-capable: %v", scopes)
	}
	if ref := session.Subject().AccountRef; ref != "" {
		t.Fatalf("installation hour must keep account_ref null: %q", ref)
	}
	mu.Lock()
	reqs := append([][]string(nil), requested...)
	calls := sessionCalls
	mu.Unlock()
	if len(reqs) != 3 {
		t.Fatalf("want 3 session requests, got %d: %v", len(reqs), reqs)
	}
	if !containsAny(reqs[0], "access:sync") || containsAny(reqs[0], "management-only") {
		t.Fatalf("first request must be preferred: %v", reqs[0])
	}
	if !containsAny(reqs[1], "management-only") || containsAny(reqs[1], "access:sync") {
		t.Fatalf("fallback request must be management-only: %v", reqs[1])
	}
	if !containsAny(reqs[2], "access:sync") || containsAny(reqs[2], "management-only") {
		t.Fatalf("post-activation request must be preferred: %v", reqs[2])
	}
	_ = calls
}

func containsAny(values []string, wanted string) bool {
	for _, v := range values {
		if v == wanted {
			return true
		}
	}
	return false
}

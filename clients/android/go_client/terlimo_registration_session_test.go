package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"wg-turn-client/accountaccess"
)

// Exercise the production registration command with a still-valid UNLINKED session.
// The server, not registration status or client code, supplies the new account identity.
func TestTelegramRefreshReauthenticatesValidUnlinkedSession(t *testing.T) {
	var confirmed atomic.Bool
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/mobile/v1/auth/challenge":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2030-01-01T00:00:00Z",
				"schema_version": "1.0", "status": "ok", "challenge_id": "00112233445566778899aabbccddeeff",
				"nonce_b64": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "purpose": "session",
				"environment": "test", "expires_at": "2030-01-01T00:05:00Z", "single_use": true})
		case "/api/mobile/v1/auth/session":
			calls.Add(1)
			var account any
			token := "unlinked-token"
			if confirmed.Load() {
				account = "account-from-server"
				token = "linked-token"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2030-01-01T00:00:00Z",
				"schema_version": "1.0", "status": "ok", "session": map[string]any{
					"session_id": token, "account_ref": account, "installation_ref": "inst",
					"scopes": []string{"session:read", "access:sync"}, "generation": "1",
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
		}, Now: func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := session.Bearer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if session.Subject().AccountRef != "" {
		t.Fatal("initial account must be unlinked")
	}
	confirmed.Store(true)
	cached, err := session.Bearer(context.Background())
	if err != nil || cached != before || calls.Load() != 1 {
		t.Fatal("fixture must still reuse a valid old session")
	}
	m := &managedMobile{session: session, client: &accountaccess.Client{}, runner: &accountaccess.Runner{}}
	// Cancel only the wait for the runner notification; no background runner is needed
	// to exercise the command and the next ordinary Ensure/Bearer resolution.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.handleRegistration(ctx, "refresh_telegram_registration")
	if session.Subject().AccountRef != "" {
		t.Fatal("refresh must not synthesize account identity")
	}
	after, err := session.Bearer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after == before || calls.Load() != 2 {
		t.Fatal("refresh must authenticate again despite valid old expiry")
	}
	if session.Subject().AccountRef != "account-from-server" {
		t.Fatal("identity must come from fresh session response")
	}
}

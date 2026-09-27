package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wg-turn-client/accountaccess"
)

// Focused wiring test: the real managed bridge carries the account_access event built by
// the single native coordinator, authenticated through the embedded mobile-v1 session
// against a controlled local HTTP fixture (no live endpoint).

type wiringFixture struct {
	mu          sync.Mutex
	key         *ecdsa.PrivateKey
	spkiDER     []byte
	challenges  map[string]string
	requests    int
	bearerToken string
}

func newWiringFixture(t *testing.T) *wiringFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &wiringFixture{key: key, spkiDER: der, challenges: map[string]string{}}
}

func (f *wiringFixture) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/mobile/v1/auth/challenge":
			challengeID := wiringRandomHex(16)
			nonce := accountaccess.B64URLEncode(wiringRandomBytes(32))
			f.mu.Lock()
			f.challenges[challengeID] = nonce
			f.mu.Unlock()
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2026-09-21T12:00:00Z",
				"schema_version": "1.0", "status": "ok", "challenge_id": challengeID, "nonce_b64": nonce,
				"purpose": body["purpose"], "environment": body["environment"],
				"expires_at": "2026-09-21T12:05:00Z", "single_use": true,
			})
		case "/api/mobile/v1/auth/session":
			proof, _ := body["proof"].(map[string]any)
			if !f.verifyProof(proof) {
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = writer.Write([]byte(`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"PROOF_INVALID","retryable":false}`))
				return
			}
			f.mu.Lock()
			f.requests++
			if f.bearerToken == "" {
				f.bearerToken = wiringRandomHex(16)
			}
			token := f.bearerToken
			f.mu.Unlock()
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2026-09-21T12:00:00Z",
				"schema_version": "1.0", "status": "ok",
				"session": map[string]any{"session_id": token, "account_ref": nil,
					"installation_ref": accountaccess.InstallationFingerprint(f.spkiDER),
					"scopes":           []string{"session:read"}, "generation": "1",
					"issued_at": "2026-09-21T12:00:00Z", "expires_at": "2030-01-01T00:00:00Z"},
			})
		case "/api/mobile/v1/me":
			if request.Header.Get("Authorization") != "Bearer "+f.bearerToken {
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = writer.Write([]byte(`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"SESSION_EXPIRED","retryable":false}`))
				return
			}
			_, _ = writer.Write([]byte(meFixtureJSON))
		case "/api/mobile/v1/gateways":
			_, _ = writer.Write([]byte(catalogFixtureJSON))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	})
}

func (f *wiringFixture) verifyProof(proof map[string]any) bool {
	if len(proof) != 8 {
		return false
	}
	challengeID, _ := proof["challenge_id"].(string)
	nonce, _ := proof["nonce_b64"].(string)
	requestID, _ := proof["request_id"].(string)
	signed, _ := proof["signed_payload_b64"].(string)
	f.mu.Lock()
	expected := f.challenges[challengeID]
	f.mu.Unlock()
	if expected == "" || expected != nonce {
		return false
	}
	raw, payload, err := accountaccess.DecodeProofPayload(signed)
	if err != nil || payload["request_id"] != requestID || payload["nonce"] != nonce {
		return false
	}
	message, err := accountaccess.POPMessage(requestID, challengeID, nonce, raw)
	if err != nil {
		return false
	}
	signature, err := accountaccess.B64URLDecodeStrict(fmt.Sprint(proof["signature_b64"]), -1)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(message)
	return ecdsa.VerifyASN1(&f.key.PublicKey, digest[:], signature)
}

func wiringRandomHex(size int) string {
	return fmt.Sprintf("%x", wiringRandomBytes(size))
}

func wiringRandomBytes(size int) []byte {
	raw := make([]byte, size)
	_, _ = rand.Read(raw)
	return raw
}

const meFixtureJSON = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"ok","account_state":"ACTIVE_TRIAL","telegram_linked":true,"entitlement":{"type":"trial","status":"active","valid_from":"2026-09-21T09:00:00Z","valid_until":"2026-09-28T09:00:00Z","effective_device_limit":2,"slots_used":1,"revision":"3","perpetual_commercial":false},"binding_status":"none","binding_revision":null,"management_only":false,"onboarding":{"state":"not_started","started_by":"server_confirmed_first_connection","started_at":null,"not_after":null,"duration_seconds":3600,"one_time":true,"extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,"requires_hardware_id":false,"unit":"installation_fingerprint","post_telegram_identity":"account_history_correlation","pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},"grant_resolution":{"control_available":true,"restricted_checkout_available":true,"data_access":"none","effective_deadline":null},"revision":"7"}`

const catalogFixtureJSON = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"ok","revision":"4","valid_until":"2026-09-21T12:10:00Z","issued_at":"2026-09-21T11:55:00Z","gateways":[{"gateway_id":"gw-1","name":"Synthetic","region":"test","country_code":"XX","capabilities":["managed"],"target_workers":36,"transport":{"protocol":"wdtt-v17","peer_ip":"127.0.0.1","dtls_port":56300,"wg_port":56302,"dtls_spki_sha256":"PAyFy4YlbMsIfA4GfHvX_r8-HUWKVuj9PUeJ4pEtAjo"},"access":{"grant_id":"grant-1","device_ref":"dev-1","password":"synthetic","generation":"1","lease_seq":"1","not_after":"2026-09-21T12:10:00Z"}}]}`

func TestManagedMobileEmitsAccountAccessThroughRealBridge(t *testing.T) {
	fixture := newWiringFixture(t)
	server := httptest.NewServer(fixture.handler())
	defer server.Close()

	var output bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge := newManagedBridge(&output, "attempt", cancel)
	start := managedStart{V: 1, Type: "start", AttemptID: "attempt",
		MobileBaseURL: server.URL, MobileEnvironment: "test"}
	signer := func(ctx context.Context, transcript []byte) ([]byte, error) {
		digest := sha256.Sum256(transcript)
		return ecdsa.SignASN1(rand.Reader, fixture.key, digest[:])
	}
	controller := &managedController{bridge: bridge, start: managedStart{MobileBaseURL: start.MobileBaseURL}}
	mobile, err := newManagedMobile(start, fixture.spkiDER, signer, bridge, controller)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		mobile.run(ctx, bridge)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(output.String(), `"type":"account_access"`) {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("account_access never reached the bridge: %q", output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	found := false
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if !strings.Contains(line, `"type":"account_access"`) {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("account_access line is not JSON: %v", err)
		}
		// S3-A registration and S3-B trial are the accepted additive display blocks.
		wanted := []string{"v", "attempt_id", "type", "access_version", "server_time", "session_generation",
			"previous_session_generation", "access_revision", "account", "entitlement", "onboarding", "grant_resolution",
			"registration", "trial"}
		if len(event) != len(wanted) {
			t.Fatalf("frozen key count=%d want %d: %s", len(event), len(wanted), line)
		}
		for _, key := range wanted {
			if _, present := event[key]; !present {
				t.Fatalf("missing frozen key %q in %s", key, line)
			}
		}
		if event["access_revision"] != "7" || event["session_generation"] != "1" ||
			event["previous_session_generation"] != nil {
			t.Fatalf("unexpected projection values: %s", line)
		}
		found = true
	}
	if !found {
		t.Fatalf("account_access did not reach the bridge output: %q", output.String())
	}
	fixture.mu.Lock()
	requests := fixture.requests
	fixture.mu.Unlock()
	if requests == 0 {
		t.Fatal("session challenge/session flow did not run")
	}
}

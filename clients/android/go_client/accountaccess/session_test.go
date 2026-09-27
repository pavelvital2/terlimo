package accountaccess

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Focused fixture for the embedded mobile-v1 session: it verifies the exact PoP
// transcript/signature the client produces and reproduces the server scope negotiation.

type authFixture struct {
	mu               sync.Mutex
	key              *ecdsa.PrivateKey
	spkiB64          string
	spkiDER          []byte
	challenges       map[string]string // challenge_id -> nonce_b64
	installations    map[string]bool   // fingerprint -> enrolled
	linked           bool
	enrollState      string
	sessions         map[string]time.Time // token -> expiry
	issuedScopes     map[string][]string  // token -> issued scopes
	sessionCalls     int
	lastScopes       []string
	lastDeniedScopes []string
	proofsVerified   int
	catalogRaw       []byte
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	catalogRaw, err := os.ReadFile("testdata/accepted-253cd8a2/catalog_response.json")
	if err != nil {
		t.Fatalf("accepted catalog fixture: %v", err)
	}
	return &authFixture{
		key:           key,
		spkiB64:       B64URLEncode(der),
		spkiDER:       der,
		challenges:    map[string]string{},
		installations: map[string]bool{},
		sessions:      map[string]time.Time{},
		issuedScopes:  map[string][]string{},
		catalogRaw:    catalogRaw,
	}
}

func (f *authFixture) errorResponse(writer http.ResponseWriter, status int, code, reason string) {
	body := map[string]any{
		"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2026-09-21T12:00:00Z",
		"schema_version": "1.0", "status": "error", "code": code, "retryable": false,
	}
	if reason != "" {
		body["details"] = map[string]any{"reason": reason}
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

func (f *authFixture) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if request.Body != nil {
			_ = json.NewDecoder(request.Body).Decode(&body)
		}
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/api/mobile/v1/auth/challenge":
			if len(body) != 3 {
				f.errorResponse(writer, 400, "BAD_MESSAGE", "")
				return
			}
			challengeID := randomFixtureHex(16)
			nonce := randomFixtureBase64(32)
			f.mu.Lock()
			f.challenges[challengeID] = nonce
			f.mu.Unlock()
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2026-09-21T12:00:00Z",
				"schema_version": "1.0", "status": "ok", "challenge_id": challengeID, "nonce_b64": nonce,
				"purpose": body["purpose"], "environment": body["environment"],
				"expires_at": "2026-09-21T12:05:00Z", "single_use": true,
			})
		case request.URL.Path == "/api/mobile/v1/installations":
			if !f.verifyProof(body) {
				f.errorResponse(writer, 401, "PROOF_INVALID", "")
				return
			}
			fingerprint := InstallationFingerprint(f.spkiDER)
			f.mu.Lock()
			f.installations[fingerprint] = true
			f.mu.Unlock()
			state := f.enrollState
			if state == "" {
				state = "technical"
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2026-09-21T12:00:00Z",
				"schema_version": "1.0", "status": "ok",
				"installation": map[string]any{"installation_id": fingerprint,
					"public_key_spki_b64": "unused", "fingerprint": fingerprint, "platform": "android",
					"environment": "test", "state": state},
				"session": map[string]any{"session_id": "enrollment-token", "account_ref": nil,
					"installation_ref": fingerprint, "scopes": []string{"enrollment"},
					"generation": "1", "issued_at": "2026-09-21T12:00:00Z", "expires_at": "2026-09-21T13:00:00Z"},
				"entitlement_created": false,
			})
		case request.URL.Path == "/api/mobile/v1/auth/session":
			if request.Header.Get("Idempotency-Key") == "" {
				f.errorResponse(writer, 400, "BAD_MESSAGE", "idempotency_key_required")
				return
			}
			payload := f.verifyProofPayload(body)
			if payload == nil {
				f.errorResponse(writer, 401, "PROOF_INVALID", "installation_unknown")
				return
			}
			fingerprint, _ := payload["installation_id"].(string)
			f.mu.Lock()
			enrolled := f.installations[fingerprint]
			f.proofsVerified++
			f.mu.Unlock()
			if !enrolled {
				f.errorResponse(writer, 401, "PROOF_INVALID", "installation_unknown")
				return
			}
			requested := toStrings(payload["requested_scopes"])
			allowed := []string{"session:read", "session:write", "management-only", "enrollment"}
			if f.linked {
				allowed = append(allowed, "account:read", "access:sync")
			}
			for _, scope := range requested {
				if !contains(allowed, scope) {
					f.mu.Lock()
					f.lastDeniedScopes = append([]string(nil), requested...)
					f.mu.Unlock()
					f.errorResponse(writer, 403, "ACCESS_DENIED", "scope_not_available_for_account_state")
					return
				}
			}
			token := randomFixtureHex(16)
			f.mu.Lock()
			f.sessionCalls++
			f.lastScopes = requested
			f.sessions[token] = time.Now().Add(time.Hour)
			f.issuedScopes[token] = append([]string(nil), requested...)
			linked := f.linked
			f.mu.Unlock()
			var accountRef any
			if linked {
				accountRef = "acc-fixture-1"
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2026-09-21T12:00:00Z",
				"schema_version": "1.0", "status": "ok",
				"session": map[string]any{"session_id": token, "account_ref": accountRef,
					"installation_ref": fingerprint, "scopes": requested, "generation": "1",
					"issued_at": "2026-09-21T12:00:00Z", "expires_at": "2026-09-21T13:00:00Z"},
			})
		case request.URL.Path == "/api/mobile/v1/gateways":
			// Real semantics mirror: the data route judges the issued session scopes, so a
			// session carrying management-only is rejected 403 even when a data right exists.
			token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
			f.mu.Lock()
			scopes := append([]string(nil), f.issuedScopes[token]...)
			f.mu.Unlock()
			if token == "" || len(scopes) == 0 {
				f.errorResponse(writer, 401, "SESSION_INVALID", "")
				return
			}
			if contains(scopes, "management-only") {
				f.errorResponse(writer, 403, "ACCESS_DENIED", "management_only_has_no_data_access")
				return
			}
			_, _ = writer.Write(f.catalogRaw)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	})
}

func (f *authFixture) verifyProof(body map[string]any) bool {
	proof, _ := body["proof"].(map[string]any)
	return f.verifyProofWithKey(proof, f.spkiB64) != nil
}

func (f *authFixture) verifyProofPayload(body map[string]any) map[string]any {
	proof, _ := body["proof"].(map[string]any)
	spki := f.spkiB64
	return f.verifyProofWithKey(proof, spki)
}

// verifyProofWithKey enforces the exact canonical transcript, canonical payload bytes and
// the accepted signature; it returns the payload on success.
func (f *authFixture) verifyProofWithKey(proof map[string]any, spkiB64 string) map[string]any {
	if len(proof) != 8 || proof["algorithm"] != "ES256" {
		return nil
	}
	challengeID, _ := proof["challenge_id"].(string)
	nonce, _ := proof["nonce_b64"].(string)
	requestID, _ := proof["request_id"].(string)
	signed, _ := proof["signed_payload_b64"].(string)
	payloadHash, _ := proof["payload_hash"].(string)
	signature, _ := proof["signature_b64"].(string)
	f.mu.Lock()
	expectedNonce := f.challenges[challengeID]
	f.mu.Unlock()
	if expectedNonce == "" || expectedNonce != nonce {
		return nil
	}
	raw, payload, err := DecodeProofPayload(signed)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != payloadHash {
		return nil
	}
	if payload["request_id"] != requestID || payload["nonce"] != nonce {
		return nil
	}
	message, err := POPMessage(requestID, challengeID, nonce, raw)
	if err != nil {
		return nil
	}
	der, err := B64URLDecodeStrict(spkiB64, -1)
	if err != nil {
		return nil
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil
	}
	public, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil
	}
	signatureRaw, err := B64URLDecodeStrict(signature, -1)
	if err != nil {
		return nil
	}
	digest := sha256.Sum256(message)
	if !ecdsa.VerifyASN1(public, digest[:], signatureRaw) {
		return nil
	}
	return payload
}

func randomFixtureHex(size int) string {
	raw := make([]byte, size)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

func randomFixtureBase64(size int) string {
	raw := make([]byte, size)
	_, _ = rand.Read(raw)
	return B64URLEncode(raw)
}

func mustDecode(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := B64URLDecodeStrict(value, -1)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func toStrings(value any) []string {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		text, _ := item.(string)
		out = append(out, text)
	}
	return out
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func newFixtureSession(t *testing.T, fixture *authFixture, server *httptest.Server) *MobileSession {
	t.Helper()
	session, err := NewMobileSession(MobileConfig{
		BaseURL:     server.URL,
		Environment: EnvironmentTest,
		SPKIDER:     mustDecode(t, fixture.spkiB64),
		HTTP:        server.Client(),
		Signer: func(ctx context.Context, transcript []byte) ([]byte, error) {
			digest := sha256.Sum256(transcript)
			return ecdsa.SignASN1(rand.Reader, fixture.key, digest[:])
		},
		Now: func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestMobileSessionEnrollmentThenSessionFallsBackToUnlinkedScopes(t *testing.T) {
	fixture := newAuthFixture(t)
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	session := newFixtureSession(t, fixture, server)

	token, err := session.Bearer(context.Background())
	if err != nil {
		t.Fatalf("bearer: %v", err)
	}
	if token == "" || token == "enrollment-token" {
		t.Fatalf("unexpected token %q", token)
	}
	if session.Fingerprint() != InstallationFingerprint(mustDecode(t, fixture.spkiB64)) {
		t.Fatal("fingerprint mismatch")
	}
	if subject := session.Subject(); subject.InstallationID != session.Fingerprint() || subject.AccountRef != "" {
		t.Fatalf("unlinked subject wrong: %+v", subject)
	}
	if !contains(session.Scopes(), "session:read") || !contains(session.Scopes(), "session:write") ||
		contains(session.Scopes(), "access:sync") {
		t.Fatalf("unlinked scopes wrong: %v", session.Scopes())
	}
	if fixture.sessionCalls != 1 || !contains(fixture.lastScopes, "session:read") ||
		!contains(fixture.lastScopes, "session:write") ||
		contains(fixture.lastScopes, "access:sync") {
		t.Fatalf("only the unlinked session may be issued: calls=%d scopes=%v", fixture.sessionCalls, fixture.lastScopes)
	}
}

func TestMobileSessionAcceptsBoundReenrollmentAndRejectsRevoked(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.installations[InstallationFingerprint(fixture.spkiDER)] = true
	fixture.enrollState = "bound"
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	session := newFixtureSession(t, fixture, server)
	if _, err := session.Bearer(context.Background()); err != nil {
		t.Fatalf("bound re-enrollment is a valid contract response: %v", err)
	}

	revokedFixture := newAuthFixture(t)
	revokedFixture.enrollState = "revoked"
	revokedServer := httptest.NewServer(revokedFixture.handler())
	defer revokedServer.Close()
	revokedSession := newFixtureSession(t, revokedFixture, revokedServer)
	if _, err := revokedSession.Bearer(context.Background()); err == nil {
		t.Fatal("revoked installation state must never be accepted")
	}
}

func TestMobileSessionLinkedGrantCarriesAccessSyncAndAccountRef(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.installations[InstallationFingerprint(mustDecode(t, fixture.spkiB64))] = true
	fixture.linked = true
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	session := newFixtureSession(t, fixture, server)

	if _, err := session.Bearer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !contains(session.Scopes(), "access:sync") {
		t.Fatalf("linked scopes must include access:sync: %v", session.Scopes())
	}
	if subject := session.Subject(); subject.AccountRef != "acc-fixture-1" {
		t.Fatalf("linked subject wrong: %+v", subject)
	}
	if fixture.sessionCalls != 1 {
		t.Fatalf("linked session must not need the fallback: %d calls", fixture.sessionCalls)
	}
}

func TestMobileSessionRefreshesAfterServerExpiry(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.installations[InstallationFingerprint(mustDecode(t, fixture.spkiB64))] = true
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	session, err := NewMobileSession(MobileConfig{
		BaseURL: server.URL, Environment: EnvironmentTest, SPKIDER: fixture.spkiDER,
		HTTP: server.Client(),
		Signer: func(ctx context.Context, transcript []byte) ([]byte, error) {
			digest := sha256.Sum256(transcript)
			return ecdsa.SignASN1(rand.Reader, fixture.key, digest[:])
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := session.Bearer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Server-side invalidation: a route would answer SESSION_EXPIRED, the caller refreshes.
	session.Refresh()
	second, err := session.Bearer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	calls := fixture.sessionCalls
	fixture.mu.Unlock()
	if second == first || calls != 2 {
		t.Fatalf("explicit refresh must re-authenticate: %q -> %q calls %d", first, second, calls)
	}
	// Stored expiry also forces re-authentication on the next Ensure.
	session.mu.Lock()
	session.current.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	session.mu.Unlock()
	if _, err := session.Bearer(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	calls = fixture.sessionCalls
	fixture.mu.Unlock()
	if calls != 3 {
		t.Fatalf("expired stored session must re-authenticate: calls %d", calls)
	}
}

func TestMobileSessionPlaintextPolicy(t *testing.T) {
	spki := make([]byte, 91)
	if _, err := NewMobileSession(MobileConfig{BaseURL: "http://example.com", Environment: EnvironmentTest,
		SPKIDER: spki, HTTP: http.DefaultClient, Signer: func(context.Context, []byte) ([]byte, error) { return nil, nil }}); err == nil {
		t.Fatal("plaintext non-loopback base url must be rejected")
	}
	if _, err := NewMobileSession(MobileConfig{BaseURL: "http://127.0.0.1:8080", Environment: EnvironmentTest,
		SPKIDER: spki, HTTP: http.DefaultClient, Signer: func(context.Context, []byte) ([]byte, error) { return nil, nil }}); err != nil {
		t.Fatalf("loopback fixture override must be accepted: %v", err)
	}
	if _, err := NewMobileSession(MobileConfig{BaseURL: "https://terlimo.193-5-251-217.sslip.io", Environment: EnvironmentTest,
		SPKIDER: spki, HTTP: http.DefaultClient, Signer: func(context.Context, []byte) ([]byte, error) { return nil, nil }}); err != nil {
		t.Fatalf("seed https base url must be accepted: %v", err)
	}
}

func TestMobileSessionServerNoBindingStaysUnlinked(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.installations[InstallationFingerprint(mustDecode(t, fixture.spkiB64))] = true
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	session := newFixtureSession(t, fixture, server)
	if _, err := session.Bearer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if session.Subject().AccountRef != "" || contains(session.Scopes(), "access:sync") {
		t.Fatalf("no trusted link must stay unlinked: %+v %v", session.Subject(), session.Scopes())
	}
}

func TestMobileSessionScopeSetsContract(t *testing.T) {
	if len(preferredScopes) != 3 || !contains(preferredScopes, "session:read") ||
		!contains(preferredScopes, "session:write") ||
		!contains(preferredScopes, "access:sync") || contains(preferredScopes, "management-only") {
		t.Fatalf("preferred scopes must be data-capable without management-only: %v", preferredScopes)
	}
	if len(unlinkedScopes) != 3 || !contains(unlinkedScopes, "session:read") ||
		!contains(unlinkedScopes, "session:write") ||
		!contains(unlinkedScopes, "management-only") || contains(unlinkedScopes, "access:sync") {
		t.Fatalf("unlinked scopes must keep their prior meaning and limits: %v", unlinkedScopes)
	}
}

func TestMobileSessionLinkedCatalogAdmissionUsesDataScopes(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.installations[InstallationFingerprint(mustDecode(t, fixture.spkiB64))] = true
	fixture.linked = true
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	session := newFixtureSession(t, fixture, server)

	if _, err := session.Bearer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scopes := session.Scopes(); contains(scopes, "management-only") || !contains(scopes, "access:sync") ||
		!contains(scopes, "session:write") {
		t.Fatalf("linked session must be data-capable: %v", scopes)
	}
	fixture.mu.Lock()
	calls := fixture.sessionCalls
	last := append([]string(nil), fixture.lastScopes...)
	verified := fixture.proofsVerified
	fixture.mu.Unlock()
	if calls != 1 || contains(last, "management-only") {
		t.Fatalf("linked preferred request must be granted directly: calls=%d scopes=%v", calls, last)
	}
	if verified == 0 {
		t.Fatal("the session proof must be verified by the fixture")
	}

	client := &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session}
	gateways, apiError, err := client.GetGateways(context.Background())
	if err != nil {
		t.Fatalf("catalog transport: %v", err)
	}
	if apiError != nil {
		t.Fatalf("data route denied a data-capable session: %+v", apiError)
	}
	if gateways.Catalog == nil || gateways.Catalog.Status != "ok" || len(gateways.Catalog.Gateways) == 0 {
		t.Fatalf("catalog not admitted: %+v", gateways.Catalog)
	}
}

func TestMobileSessionUnboundFallbackStaysManagementOnlyAndDeniedData(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.installations[InstallationFingerprint(mustDecode(t, fixture.spkiB64))] = true
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	session := newFixtureSession(t, fixture, server)

	if _, err := session.Bearer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scopes := session.Scopes(); !contains(scopes, "management-only") || !contains(scopes, "session:write") ||
		contains(scopes, "access:sync") {
		t.Fatalf("unbound fallback must stay management-only: %v", scopes)
	}
	fixture.mu.Lock()
	calls := fixture.sessionCalls
	denied := append([]string(nil), fixture.lastDeniedScopes...)
	fixture.mu.Unlock()
	if calls != 1 || !contains(denied, "access:sync") || contains(denied, "management-only") {
		t.Fatalf("preferred data request must be denied, then fall back: calls=%d denied=%v", calls, denied)
	}

	client := &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session}
	gateways, apiError, err := client.GetGateways(context.Background())
	if err != nil {
		t.Fatalf("catalog transport: %v", err)
	}
	if apiError == nil || apiError.Code != "ACCESS_DENIED" {
		t.Fatalf("management-only session must be denied the data route: gateways=%+v apiError=%+v", gateways, apiError)
	}
	if gateways.Catalog != nil || gateways.Browse != nil {
		t.Fatalf("denied catalog must stay empty: %+v", gateways)
	}
}

func TestMobileSessionAttachRenewalGetsDataCapableSession(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.installations[InstallationFingerprint(mustDecode(t, fixture.spkiB64))] = true
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	session := newFixtureSession(t, fixture, server)
	client := &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session}

	oldToken, err := session.Bearer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, apiError, getErr := client.GetGateways(context.Background()); getErr != nil || apiError == nil {
		t.Fatalf("unlinked session must be denied data: apiError=%+v err=%v", apiError, getErr)
	}

	// The trusted link appears; the already issued management-only session is not re-evaluated.
	fixture.mu.Lock()
	fixture.linked = true
	fixture.mu.Unlock()
	if _, apiError, _ := client.GetGateways(context.Background()); apiError == nil {
		t.Fatal("old management-only session must not become data-capable by itself")
	}

	session.Refresh()
	newToken, err := session.Bearer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if newToken == oldToken {
		t.Fatal("renewal must not reuse the management-only token")
	}
	if scopes := session.Scopes(); contains(scopes, "management-only") || !contains(scopes, "access:sync") {
		t.Fatalf("renewed linked session must be data-capable: %v", scopes)
	}
	gateways, apiError, err := client.GetGateways(context.Background())
	if err != nil || apiError != nil || gateways.Catalog == nil || gateways.Catalog.Status != "ok" || len(gateways.Catalog.Gateways) == 0 {
		t.Fatalf("renewed session must admit the catalog: gateways=%+v apiError=%+v err=%v", gateways, apiError, err)
	}
	fixture.mu.Lock()
	calls := fixture.sessionCalls
	fixture.mu.Unlock()
	if calls != 2 {
		t.Fatalf("expected the unlinked grant then the linked renewal: calls=%d", calls)
	}
}

func TestMobileSessionChallengeEnvelopeStrict(t *testing.T) {
	fixture := newAuthFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	session := newFixtureSession(t, fixture, server)
	if _, err := session.Bearer(context.Background()); err == nil || !strings.Contains(err.Error(), "challenge") {
		t.Fatalf("malformed challenge envelope must fail explicitly: %v", err)
	}
}

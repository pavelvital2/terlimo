package accountaccess

// Mobile v1 embedded session: challenge -> enrollment -> session by installation key.
// The P-256 installation key stays in the host Keystore; native only submits transcripts
// to the existing signing bridge. No manual bearer injection and no plaintext fallback.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Signer signs the exact WLBS-POP-1 transcript (host bridge in production).
type Signer func(ctx context.Context, transcript []byte) ([]byte, error)

// MobileConfig configures the embedded session client.
type MobileConfig struct {
	BaseURL     string
	Environment PoPEnvironment
	SPKIDER     []byte
	Platform    string
	Name        string
	HTTP        Doer
	Signer      Signer
	Now         func() time.Time
}

type challengeResponse struct {
	RequestID     string `json:"request_id"`
	ServerTime    string `json:"server_time"`
	SchemaVersion string `json:"schema_version"`
	Status        string `json:"status"`
	ChallengeID   string `json:"challenge_id"`
	NonceB64      string `json:"nonce_b64"`
	Purpose       string `json:"purpose"`
	Environment   string `json:"environment"`
	ExpiresAt     string `json:"expires_at"`
	SingleUse     bool   `json:"single_use"`
}

type sessionObject struct {
	SessionID       string   `json:"session_id"`
	AccountRef      *string  `json:"account_ref"`
	InstallationRef string   `json:"installation_ref"`
	Scopes          []string `json:"scopes"`
	Generation      string   `json:"generation"`
	IssuedAt        string   `json:"issued_at"`
	ExpiresAt       string   `json:"expires_at"`
}

// enrollmentResponse mirrors the accepted EnrollmentResponse schema: the full
// installation object plus the envelope. installation_id is the server-side reference;
// the installation fingerprint is its own required field.
type enrollmentResponse struct {
	RequestID     string `json:"request_id"`
	ServerTime    string `json:"server_time"`
	SchemaVersion string `json:"schema_version"`
	Status        string `json:"status"`
	Installation  struct {
		InstallationID   string  `json:"installation_id"`
		PublicKeySPKIB64 string  `json:"public_key_spki_b64"`
		Fingerprint      string  `json:"fingerprint"`
		Platform         string  `json:"platform"`
		Name             *string `json:"name"`
		Environment      string  `json:"environment"`
		State            string  `json:"state"`
		CreatedAt        string  `json:"created_at"`
		LastSeenAt       string  `json:"last_seen_at"`
	} `json:"installation"`
	Session            sessionObject `json:"session"`
	EntitlementCreated bool          `json:"entitlement_created"`
}

// sessionResponse mirrors the accepted SessionResponse envelope.
type sessionResponse struct {
	RequestID     string        `json:"request_id"`
	ServerTime    string        `json:"server_time"`
	SchemaVersion string        `json:"schema_version"`
	Status        string        `json:"status"`
	Session       sessionObject `json:"session"`
}

// MobileSession owns the embedded challenge/enrollment/session lifecycle.
type MobileSession struct {
	config MobileConfig

	mu         sync.Mutex
	current    *sessionObject
	requests   int
	stageStart time.Time // monotonic start of the current authentication chronology
}

// unlinkedScopes are the minimal scopes available before a trusted link exists.
var unlinkedScopes = []string{"session:read", "session:write", "management-only"}

// preferredScopes asks for session:read, session:write and access:sync; the server grants
// access:sync only when a trusted active binding exists. ACCESS_DENIED falls back to
// unlinkedScopes. It must not
// carry management-only: the backend marks such a session management-only and its data
// routes reject it (403), even when a data right exists.
var preferredScopes = []string{"session:read", "session:write", "access:sync"}

// NewMobileSession validates the endpoint policy and configuration.
func NewMobileSession(config MobileConfig) (*MobileSession, error) {
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("mobile base url invalid")
	}
	if parsed.Scheme != "https" && !loopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("mobile base url must be https (plaintext is a test-only loopback override)")
	}
	if len(config.SPKIDER) == 0 {
		return nil, fmt.Errorf("installation spki required")
	}
	if config.HTTP == nil || config.Signer == nil {
		return nil, fmt.Errorf("mobile http transport and signer required")
	}
	if config.Environment != EnvironmentTest && config.Environment != EnvironmentProduction {
		return nil, fmt.Errorf("mobile environment invalid")
	}
	if config.Platform == "" {
		config.Platform = "android"
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	return &MobileSession{config: config}, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Fingerprint is the contract installation fingerprint (SHA-256 of SPKI DER, hex).
func (m *MobileSession) Fingerprint() string {
	return InstallationFingerprint(m.config.SPKIDER)
}

// Ensure returns a valid bearer, authenticating or refreshing when needed.
func (m *MobileSession) Ensure(ctx context.Context) error {
	m.mu.Lock()
	current := m.current
	m.mu.Unlock()
	if current != nil {
		expires, err := time.Parse(time.RFC3339, current.ExpiresAt)
		if err == nil && m.config.Now().Add(30*time.Second).Before(expires) {
			return nil
		}
	}
	_, err := m.authenticate(ctx)
	return err
}

// Bearer implements TokenSource; it never returns a stale token.
func (m *MobileSession) Bearer(ctx context.Context) (string, error) {
	if err := m.Ensure(ctx); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return "", fmt.Errorf("mobile session unavailable")
	}
	return m.current.SessionID, nil
}

// Subject is the stable authenticated subject for the revision/digest domain.
func (m *MobileSession) Subject() Subject {
	m.mu.Lock()
	defer m.mu.Unlock()
	accountRef := ""
	if m.current != nil && m.current.AccountRef != nil {
		accountRef = *m.current.AccountRef
	}
	return Subject{AccountRef: accountRef, InstallationID: m.Fingerprint()}
}

// Scopes returns the granted scopes of the current session.
func (m *MobileSession) Scopes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return nil
	}
	return append([]string(nil), m.current.Scopes...)
}

// Generation returns the current session generation (equality correlation only).
func (m *MobileSession) Generation() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return ""
	}
	return m.current.Generation
}

// authenticate runs challenge + proof; enrollment happens only when the server proves the
// installation is unknown.
func (m *MobileSession) authenticate(ctx context.Context) (*sessionObject, error) {
	m.stageBegin()
	session, apiError, err := m.trySession(ctx, preferredScopes)
	if err == nil && apiError == nil {
		return session, nil
	}
	if err != nil {
		return nil, err
	}
	if apiError.Code == "PROOF_INVALID" {
		if reason, _ := apiError.Details["reason"].(string); reason == "installation_unknown" {
			if enrollErr := m.enroll(ctx); enrollErr != nil {
				return nil, enrollErr
			}
			session, apiError, err = m.trySession(ctx, preferredScopes)
			if err == nil && apiError == nil {
				return session, nil
			}
			if err != nil {
				return nil, err
			}
		}
	}
	if apiError != nil && apiError.Code == "ACCESS_DENIED" {
		// No trusted link: the same purpose is retried with the unlinked scope set.
		session, apiError, err = m.trySession(ctx, unlinkedScopes)
		if err == nil && apiError == nil {
			return session, nil
		}
		if err != nil {
			return nil, err
		}
	}
	if apiError != nil {
		return nil, fmt.Errorf("mobile session failed: %s", apiError.Code)
	}
	return nil, fmt.Errorf("mobile session failed")
}

// Refresh drops the current session so the next Ensure authenticates with a fresh proof.
// It is called when a route reports SESSION_EXPIRED/SESSION_INVALID.
func (m *MobileSession) Refresh() {
	m.mu.Lock()
	m.current = nil
	m.mu.Unlock()
}

func (m *MobileSession) trySession(ctx context.Context, scopes []string) (*sessionObject, *ErrorResponse, error) {
	challenge, apiError, err := m.requestChallenge(ctx, "session")
	if err != nil || apiError != nil {
		return nil, apiError, err
	}
	requestID, err := NewRequestID()
	if err != nil {
		return nil, nil, err
	}
	idempotencyKey, err := NewRequestID()
	if err != nil {
		return nil, nil, err
	}
	payload := map[string]any{
		"env":              string(m.config.Environment),
		"scope":            "session",
		"op":               "auth.session",
		"installation_id":  m.Fingerprint(),
		"ts":               m.config.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"nonce":            challenge.NonceB64,
		"request_id":       requestID,
		"idempotency_key":  idempotencyKey,
		"requested_scopes": append([]string(nil), scopes...),
	}
	proof, err := m.buildProof(ctx, requestID, challenge, payload)
	if err != nil {
		return nil, nil, err
	}
	body := map[string]any{"proof": proof}
	m.stage(stageSessionPostBegin)
	raw, status, err := m.post(ctx, "/auth/session", body, idempotencyKey)
	if err != nil {
		// The transport/round-trip ended without a usable response: this is an explicit
		// completion, never a received response.
		m.stage(stageSessionPostEnd)
		return nil, nil, err
	}
	m.stage(stageSessionResponse)
	if status != http.StatusOK {
		envelope, decodeErr := decodeError(raw, status)
		if decodeErr != nil {
			return nil, nil, decodeErr
		}
		return nil, envelope, nil
	}
	var response sessionResponse
	if err := decodeStrict(raw, &response); err != nil {
		return nil, nil, fmt.Errorf("session decode: %w", err)
	}
	if response.SchemaVersion != SchemaVersion || response.Status != "ok" {
		return nil, nil, fmt.Errorf("session envelope invalid")
	}
	if err := m.adopt(response.Session); err != nil {
		return nil, nil, err
	}
	return &response.Session, nil, nil
}

func (m *MobileSession) enroll(ctx context.Context) error {
	challenge, apiError, err := m.requestChallenge(ctx, "enrollment")
	if err != nil || apiError != nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("enrollment challenge failed: %s", apiError.Code)
	}
	requestID, err := NewRequestID()
	if err != nil {
		return err
	}
	payload := map[string]any{
		"env":                 string(m.config.Environment),
		"scope":               "enrollment",
		"op":                  "installations.create",
		"installation_id":     m.Fingerprint(),
		"ts":                  m.config.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"nonce":               challenge.NonceB64,
		"request_id":          requestID,
		"public_key_spki_b64": B64URLEncode(m.config.SPKIDER),
		"platform":            m.config.Platform,
	}
	if m.config.Name != "" {
		payload["name"] = m.config.Name
	}
	proof, err := m.buildProof(ctx, requestID, challenge, payload)
	if err != nil {
		return err
	}
	raw, status, err := m.post(ctx, "/installations", map[string]any{
		"public_key_spki_b64": B64URLEncode(m.config.SPKIDER),
		"proof":               proof,
	}, "")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		envelope, decodeErr := decodeError(raw, status)
		if decodeErr != nil {
			return decodeErr
		}
		return fmt.Errorf("enrollment failed: %s", envelope.Code)
	}
	var response enrollmentResponse
	if err := decodeStrict(raw, &response); err != nil {
		return fmt.Errorf("enrollment decode: %w", err)
	}
	if response.SchemaVersion != SchemaVersion || response.Status != "ok" {
		return fmt.Errorf("enrollment envelope invalid")
	}
	if response.Installation.InstallationID == "" || response.Installation.Fingerprint != m.Fingerprint() {
		return fmt.Errorf("enrollment installation mismatch")
	}
	// Accepted contract enum: technical|bound|revoked. Re-enrollment of an existing
	// non-revoked installation keeps its state (bound), which is a valid response; a
	// revoked installation is never acceptable here and never resolves VPN.
	if response.Installation.Environment != string(m.config.Environment) ||
		(response.Installation.State != "technical" && response.Installation.State != "bound") {
		return fmt.Errorf("enrollment installation state invalid")
	}
	if response.EntitlementCreated {
		return fmt.Errorf("enrollment must not create an entitlement")
	}
	return nil
}

func (m *MobileSession) requestChallenge(ctx context.Context, purpose string) (challengeResponse, *ErrorResponse, error) {
	body := map[string]any{
		"installation_fingerprint": m.Fingerprint(),
		"purpose":                  purpose,
		"environment":              string(m.config.Environment),
	}
	raw, status, err := m.post(ctx, "/auth/challenge", body, "")
	if err != nil {
		return challengeResponse{}, nil, err
	}
	m.stage(stageServiceResponseReceived)
	if status != http.StatusOK {
		envelope, decodeErr := decodeError(raw, status)
		if decodeErr != nil {
			return challengeResponse{}, nil, decodeErr
		}
		return challengeResponse{}, envelope, nil
	}
	var challenge challengeResponse
	if err := decodeStrict(raw, &challenge); err != nil {
		return challengeResponse{}, nil, fmt.Errorf("challenge decode: %w", err)
	}
	if challenge.Status != "ok" || challenge.Purpose != purpose ||
		challenge.Environment != string(m.config.Environment) ||
		challenge.ChallengeID == "" || challenge.NonceB64 == "" || !challenge.SingleUse {
		return challengeResponse{}, nil, fmt.Errorf("challenge envelope invalid")
	}
	m.stage(stageChallengeDecoded)
	return challenge, nil, nil
}

func (m *MobileSession) buildProof(ctx context.Context, requestID string, challenge challengeResponse, payload map[string]any) (map[string]any, error) {
	if err := CheckTopLevelFields(payload, KnownTopLevel); err != nil {
		return nil, err
	}
	if err := CheckCriticalFields(payload, KnownTopLevel); err != nil {
		return nil, err
	}
	canonical, err := CanonicalJSON(payload)
	if err != nil {
		return nil, err
	}
	message, err := POPMessage(requestID, challenge.ChallengeID, challenge.NonceB64, canonical)
	if err != nil {
		return nil, err
	}
	m.stage(stageSignBegin)
	signature, err := m.config.Signer(ctx, message)
	m.stage(stageSignEnd)
	if err != nil {
		return nil, err
	}
	if len(signature) < 8 || len(signature) > 80 {
		return nil, fmt.Errorf("signature shape invalid")
	}
	sum := sha256.Sum256(canonical)
	return map[string]any{
		"algorithm":          "ES256",
		"signature_b64":      B64URLEncode(signature),
		"request_id":         requestID,
		"challenge_id":       challenge.ChallengeID,
		"nonce_b64":          challenge.NonceB64,
		"payload_hash":       hex.EncodeToString(sum[:]),
		"signed_payload_b64": B64URLEncode(canonical),
		"environment":        string(m.config.Environment),
	}, nil
}

func (m *MobileSession) adopt(session sessionObject) error {
	if session.SessionID == "" || session.Generation == "" || len(session.Scopes) == 0 {
		return fmt.Errorf("session object invalid")
	}
	if len(session.Generation) > 19 {
		return fmt.Errorf("session generation invalid")
	}
	if !ValidGenerationString(session.Generation) {
		return fmt.Errorf("session generation not decimal")
	}
	parsed, err := time.Parse(time.RFC3339, session.ExpiresAt)
	if err != nil || !parsed.After(m.config.Now()) {
		return fmt.Errorf("session already expired")
	}
	m.mu.Lock()
	m.current = &session
	m.requests++
	m.mu.Unlock()
	return nil
}

// ValidGenerationString reports whether the value is a decimal generation.
func ValidGenerationString(value string) bool { return ValidGeneration(value) }

func (m *MobileSession) post(ctx context.Context, path string, body any, idempotencyKey string) ([]byte, int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.config.BaseURL+"/api/mobile/v1"+path, strings.NewReader(string(raw)))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := m.config.HTTP.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	payload := make([]byte, 0, 4096)
	buffer := make([]byte, 4096)
	total := 0
	for {
		n, readErr := response.Body.Read(buffer)
		total += n
		if total > maxResponseBytes {
			return nil, response.StatusCode, fmt.Errorf("response too large")
		}
		payload = append(payload, buffer[:n]...)
		if readErr != nil {
			break
		}
	}
	return payload, response.StatusCode, nil
}

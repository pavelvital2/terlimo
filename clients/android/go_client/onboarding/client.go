package onboarding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wg-turn-client/accountaccess"
	"wg-turn-client/wlwire"
)

const maxResponseBytes = 1 << 20

// payloadFields is the exact accepted top-level set for onboarding payloads
// (pop.KNOWN_TOP_LEVEL on the backend additionally covers request_key/intent_id).
var payloadFields = map[string]bool{
	"env": true, "scope": true, "op": true, "installation_id": true,
	"request_key": true, "intent_id": true, "ts": true, "nonce": true,
	"request_id": true, "extensions": true, "gateway_key": true,
}

// Identity is the installation key + environment used to sign intent/start payloads.
type Identity struct {
	Environment    string
	InstallationID string
	Signer         accountaccess.Signer
	Now            func() time.Time
}

func (i Identity) withDefaults() Identity {
	if i.Now == nil {
		i.Now = func() time.Time { return time.Now().UTC() }
	}
	return i
}

// Client is the production adapter that rides the accepted accountaccess.Doer
// (the service channel when a seed is configured; never a direct-HTTPS fallback
// behind a configured service) and signs the exact WLBS-POP-1 transcript.
type Client struct {
	Doer     accountaccess.Doer
	Tokens   accountaccess.TokenSource
	BaseURL  string
	Identity Identity
	// ObserveStage receives the fixed name of the failing intent stage ("challenge",
	// "sign", "post", "decode") and is never called on success. It is diagnostic-only
	// and must not influence control flow.
	ObserveStage func(stage string)
}

// NewClient validates the adapter configuration.
func NewClient(doer accountaccess.Doer, tokens accountaccess.TokenSource, baseURL string, identity Identity) (*Client, error) {
	if doer == nil || tokens == nil || strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("ONBOARDING_CLIENT_CONFIG")
	}
	identity = identity.withDefaults()
	if identity.Environment == "" || identity.InstallationID == "" || identity.Signer == nil {
		return nil, errors.New("ONBOARDING_CLIENT_CONFIG")
	}
	return &Client{Doer: doer, Tokens: tokens, BaseURL: strings.TrimRight(baseURL, "/"), Identity: identity}, nil
}

type challengeEnvelope struct {
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

// Intent performs one intent operation. Retrying with the same request_key uses a
// fresh challenge and returns the same intent state; a new request_key creates a
// new intent, so callers must keep the durable key. gatewayKey is the durable
// selected-gateway pairing: it is threaded into every intent POST and poll together
// with the request_key, and an empty value keeps the legacy bytes (the field is
// omitted, never sent empty).
func (c *Client) Intent(ctx context.Context, requestKey, gatewayKey string) (IntentPoll, *APIError, error) {
	if c == nil || c.Doer == nil || c.Tokens == nil {
		return IntentPoll{}, nil, errors.New("ONBOARDING_CLIENT_CONFIG")
	}
	if requestKey == "" || len(requestKey) > maxRequestKey {
		return IntentPoll{}, nil, errors.New("ONBOARDING_REQUEST_KEY_INVALID")
	}
	challenge, err := c.challenge(ctx, IntentPurpose)
	if err != nil {
		// challenge() already reported the fixed failing-class stage.
		return IntentPoll{}, nil, err
	}
	requestID, err := accountaccess.NewRequestID()
	if err != nil {
		c.observeStage("sign")
		return IntentPoll{}, nil, err
	}
	payload := IntentPayload(c.Identity.Environment, c.Identity.InstallationID, requestKey,
		challenge.NonceB64, requestID, c.Identity.Now().UTC().Format("2006-01-02T15:04:05Z"), gatewayKey)
	proof, err := c.sign(ctx, requestID, challenge, payload)
	if err != nil {
		c.observeStage("sign")
		return IntentPoll{}, nil, err
	}
	raw, status, err := c.post(ctx, IntentPath, map[string]any{"proof": proof})
	if err != nil {
		c.observeStage("post")
		return IntentPoll{}, nil, err
	}
	if status == http.StatusOK {
		poll, decodeErr := DecodePoll(raw)
		if decodeErr != nil {
			c.observeStage("decode")
			return IntentPoll{}, nil, decodeErr
		}
		return poll, nil, nil
	}
	apiError, decodeErr := DecodeError(raw, status)
	if decodeErr != nil {
		// The post itself returned; only the error envelope failed to decode.
		c.observeStage("decode")
		return IntentPoll{}, nil, fmt.Errorf("ONBOARDING_HTTP_%d", status)
	}
	return IntentPoll{}, apiError, nil
}

func (c *Client) observeStage(stage string) {
	if c.ObserveStage != nil {
		c.ObserveStage(stage)
	}
}

// IntentPayload builds the exact signed intent payload (rev3 section 1). gateway_key is
// the optional selected-gateway binding of the intent request: callers without a
// selection pass "" and the signed bytes stay exactly the pre-selection form (the key
// is omitted, never sent empty). The signed start payload is a different, frozen
// legacy shape and never carries it.
func IntentPayload(environment, installationID, requestKey, nonce, requestID, ts, gatewayKey string) map[string]any {
	payload := map[string]any{
		"env":             environment,
		"scope":           IntentScope,
		"op":              IntentOp,
		"installation_id": installationID,
		"request_key":     requestKey,
		"ts":              ts,
		"nonce":           nonce,
		"request_id":      requestID,
	}
	if gatewayKey != "" {
		payload["gateway_key"] = gatewayKey
	}
	return payload
}

// StartPayload builds the exact signed start payload (rev3 section 2). The start RPC
// keeps the accepted legacy field set: the selected-gateway binding lives in the
// intent request only, and adding a field here would change the server-verified bytes.
func StartPayload(environment, installationID, requestKey, intentID, nonce, requestID, ts string) map[string]any {
	payload := IntentPayload(environment, installationID, requestKey, nonce, requestID, ts, "")
	payload["op"] = StartOp
	payload["intent_id"] = intentID
	return payload
}

// BuildStartProof signs the exact start payload for the explicit RPC. It is kept
// next to the intent signing so both use the same canonical PoP mechanism.
func BuildStartProof(ctx context.Context, identity Identity, requestID string, challenge StartChallenge,
	payload map[string]any) (StartProof, error) {
	identity = identity.withDefaults()
	if identity.Signer == nil {
		return StartProof{}, errors.New("ONBOARDING_CLIENT_CONFIG")
	}
	proof, err := signPayload(ctx, identity, requestID, challenge.ChallengeID, challenge.NonceB64, payload)
	if err != nil {
		return StartProof{}, err
	}
	return StartProof{
		Algorithm:        fmt.Sprint(proof["algorithm"]),
		Environment:      fmt.Sprint(proof["environment"]),
		RequestID:        fmt.Sprint(proof["request_id"]),
		ChallengeID:      fmt.Sprint(proof["challenge_id"]),
		NonceB64:         fmt.Sprint(proof["nonce_b64"]),
		PayloadHash:      fmt.Sprint(proof["payload_hash"]),
		SignedPayloadB64: fmt.Sprint(proof["signed_payload_b64"]),
		SignatureB64:     fmt.Sprint(proof["signature_b64"]),
	}, nil
}

// SignStartBody builds and encodes the complete start RPC body. This is the
// client-side half of the missing bootstrap transport; only a caller that owns
// the trusted assigned-gateway connection may send it.
func SignStartBody(ctx context.Context, identity Identity, intentID, requestKey string,
	challenge StartChallenge, nonce, requestID string) (StartBody, error) {
	identity = identity.withDefaults()
	payload := StartPayload(identity.Environment, identity.InstallationID, requestKey, intentID,
		nonce, requestID, identity.Now().UTC().Format("2006-01-02T15:04:05Z"))
	proof, err := BuildStartProof(ctx, identity, requestID, challenge, payload)
	if err != nil {
		return StartBody{}, err
	}
	return StartBody{V: 1, Op: StartOp, IntentID: intentID, RequestKey: requestKey, Proof: proof}, nil
}

func (c *Client) challenge(ctx context.Context, purpose string) (StartChallenge, error) {
	body := map[string]any{
		"installation_fingerprint": c.Identity.InstallationID,
		"purpose":                  purpose,
		"environment":              c.Identity.Environment,
	}
	raw, status, err := c.post(ctx, ChallengePath, body)
	if err != nil {
		c.observeStage("challenge_transport")
		return StartChallenge{}, err
	}
	if status != http.StatusOK {
		c.observeStage(challengeStatusStage(status))
		if apiError, decodeErr := DecodeError(raw, status); decodeErr == nil {
			return StartChallenge{}, apiError
		}
		return StartChallenge{}, fmt.Errorf("ONBOARDING_HTTP_%d", status)
	}
	var envelope challengeEnvelope
	if wlwire.StrictJSON(raw, &envelope) != nil {
		c.observeStage("challenge_decode")
		return StartChallenge{}, errors.New("ONBOARDING_CHALLENGE_MALFORMED")
	}
	if envelope.Status != "ok" || envelope.ChallengeID == "" || envelope.NonceB64 == "" ||
		envelope.Purpose != purpose || envelope.Environment != c.Identity.Environment {
		c.observeStage("challenge_semantic")
		return StartChallenge{}, errors.New("ONBOARDING_CHALLENGE_MALFORMED")
	}
	if _, err := ParseTime(envelope.ExpiresAt); err != nil {
		c.observeStage("challenge_semantic")
		return StartChallenge{}, errors.New("ONBOARDING_CHALLENGE_MALFORMED")
	}
	return StartChallenge{ChallengeID: envelope.ChallengeID, NonceB64: envelope.NonceB64,
		ExpiresAt: envelope.ExpiresAt}, nil
}

// challengeStatusStage maps a non-200 challenge status to one fixed class stage.
func challengeStatusStage(status int) string {
	switch {
	case status >= 400 && status < 500:
		return "challenge_status_4xx"
	case status >= 500 && status < 600:
		return "challenge_status_5xx"
	default:
		return "challenge_status_other"
	}
}

func (c *Client) sign(ctx context.Context, requestID string, challenge StartChallenge, payload map[string]any) (map[string]any, error) {
	return signPayload(ctx, c.Identity, requestID, challenge.ChallengeID, challenge.NonceB64, payload)
}

func signPayload(ctx context.Context, identity Identity, requestID, challengeID, nonceB64 string,
	payload map[string]any) (map[string]any, error) {
	if err := accountaccess.CheckTopLevelFields(payload, payloadFields); err != nil {
		return nil, err
	}
	canonical, err := accountaccess.CanonicalJSON(payload)
	if err != nil {
		return nil, err
	}
	message, err := accountaccess.POPMessage(requestID, challengeID, nonceB64, canonical)
	if err != nil {
		return nil, err
	}
	signature, err := identity.Signer(ctx, message)
	if err != nil {
		return nil, err
	}
	if len(signature) < 8 || len(signature) > 80 {
		return nil, errors.New("ONBOARDING_SIGNATURE_SHAPE")
	}
	sum := sha256.Sum256(canonical)
	return map[string]any{
		"algorithm":          "ES256",
		"signature_b64":      accountaccess.B64URLEncode(signature),
		"request_id":         requestID,
		"challenge_id":       challengeID,
		"nonce_b64":          nonceB64,
		"payload_hash":       hex.EncodeToString(sum[:]),
		"signed_payload_b64": accountaccess.B64URLEncode(canonical),
		"environment":        identity.Environment,
	}, nil
}

func (c *Client) post(ctx context.Context, path string, body any) ([]byte, int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	bearer, err := c.Tokens.Bearer(ctx)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	response, err := c.Doer.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if len(payload) > maxResponseBytes {
		return nil, response.StatusCode, errors.New("ONBOARDING_RESPONSE_TOO_LARGE")
	}
	return payload, response.StatusCode, nil
}

// Package onboarding implements the client side of the STEP03.6 explicit
// Connect onboarding hour contract, revision 3: the pre-admission intent
// (challenge -> intent -> pending/ready) and the explicit signed onboarding.start
// RPC codec. It is additive and lives behind the accepted accountaccess.Doer
// seam; it never starts the hour on its own and never falls back to direct HTTPS
// when a service channel is configured.
//
// Wire fields are pinned by contract revision 3 (sha256
// 5ca4fc00da55c43dc3f432e99614c1d00df0cb8b60f93b73cb7c4d98b338d8ae). The local
// backend integration fixture directory was not present on this host, so the
// decoders are contract-derived; see the decision note for the exact missing
// backend materials.
package onboarding

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"wg-turn-client/wlwire"
)

// Fixed contract identifiers (rev3 sections 1-2).
const (
	EnvironmentTest       = "test"
	EnvironmentProduction = "production"

	IntentPurpose = "onboarding-start-intent"
	StartPurpose  = "onboarding-start"

	IntentScope = "onboarding:start"
	IntentOp    = "onboarding.intent"
	StartOp     = "onboarding.start"

	ChallengePath = "/api/mobile/v1/auth/challenge"
	IntentPath    = "/api/mobile/v1/onboarding/intents"
)

// State is one of the contract intent states.
type State string

// Contract intent states.
const (
	StateNone    State = "none"
	StatePending State = "pending"
	StateReady   State = "ready"
	StateStarted State = "started"
	StateFailed  State = "failed"
	StateExpired State = "expired"
	StateRevoked State = "revoked"
)

// Bounded wire limits that mirror the contract.
const (
	maxRequestKey = 128
	maxIntentID   = 64
	maxReason     = 256
	maxErrorCode  = 64
)

// GatewayEndpoint is the trusted assigned gateway endpoint from the registry row.
type GatewayEndpoint struct {
	PeerIP         string `json:"peer_ip"`
	DTLSPort       int    `json:"dtls_port"`
	DTLSSPKISHA256 string `json:"dtls_spki_sha256"`
}

// Gateway is the immutable intent gateway binding.
type Gateway struct {
	NodeID   string          `json:"node_id"`
	Endpoint GatewayEndpoint `json:"endpoint"`
}

// StartChallenge is the outstanding start challenge issued by a ready poll.
type StartChallenge struct {
	ChallengeID string `json:"challenge_id"`
	NonceB64    string `json:"nonce_b64"`
	ExpiresAt   string `json:"expires_at"`
}

// Bootstrap is the one-time bootstrap credential of a ready poll; it must never
// appear in start bodies, replies or logs.
type Bootstrap struct {
	CredentialID string `json:"credential_id"`
	Secret       string `json:"secret"`
}

// IntentPoll is the decoded single-shape poll response. Only the fields of the
// active state are populated; status and state are always equal.
type IntentPoll struct {
	State          State
	IntentID       string
	RequestKey     string
	ExpiresAt      time.Time
	RetryAfter     int
	Gateway        *Gateway
	CredentialID   string
	Bootstrap      *Bootstrap
	StartChallenge *StartChallenge
	StartedAt      time.Time
	NotAfter       time.Time
	FailureReason  string
}

// pollEnvelope carries the real route wrapper fields (auth_api._error_response /
// onboarding_api.create_intent append request_id/server_time/schema_version to every
// public response, including the poll shapes). They are correlation metadata only.
type pollEnvelope struct {
	RequestID     string `json:"request_id"`
	ServerTime    string `json:"server_time"`
	SchemaVersion string `json:"schema_version"`
}

type pendingPayload struct {
	pollEnvelope
	Status     string  `json:"status"`
	State      string  `json:"state"`
	IntentID   string  `json:"intent_id"`
	RequestKey string  `json:"request_key"`
	ExpiresAt  string  `json:"expires_at"`
	RetryAfter int     `json:"retry_after"`
	Gateway    Gateway `json:"gateway"`
}

type readyPayload struct {
	pollEnvelope
	Status         string         `json:"status"`
	State          string         `json:"state"`
	IntentID       string         `json:"intent_id"`
	RequestKey     string         `json:"request_key"`
	ExpiresAt      string         `json:"expires_at"`
	Gateway        Gateway        `json:"gateway"`
	CredentialID   string         `json:"credential_id"`
	Bootstrap      Bootstrap      `json:"bootstrap"`
	StartChallenge StartChallenge `json:"start_challenge"`
}

type startedPayload struct {
	pollEnvelope
	Status       string  `json:"status"`
	State        string  `json:"state"`
	IntentID     string  `json:"intent_id"`
	RequestKey   string  `json:"request_key"`
	ExpiresAt    string  `json:"expires_at"`
	Gateway      Gateway `json:"gateway"`
	CredentialID string  `json:"credential_id"`
	StartedAt    string  `json:"started_at"`
	NotAfter     string  `json:"not_after"`
}

type failedPayload struct {
	pollEnvelope
	Status     string  `json:"status"`
	State      string  `json:"state"`
	IntentID   string  `json:"intent_id"`
	RequestKey string  `json:"request_key"`
	ExpiresAt  string  `json:"expires_at"`
	Gateway    Gateway `json:"gateway"`
	Reason     string  `json:"reason"`
}

// DecodePoll strictly decodes one intent poll response shape. Unknown fields,
// state/status mismatch and malformed times are rejected, never silently zeroed.
func DecodePoll(raw []byte) (IntentPoll, error) {
	var union map[string]any
	if wlwire.StrictJSON(raw, &union) != nil {
		return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
	}
	switch union["status"] {
	case string(StatePending):
		var payload pendingPayload
		if wlwire.StrictJSON(raw, &payload) != nil {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		if payload.State != payload.Status {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		expires, err := ParseTime(payload.ExpiresAt)
		if err != nil {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		gateway, err := validateGateway(payload.Gateway)
		if err != nil {
			return IntentPoll{}, err
		}
		if err := validateIdentity(payload.IntentID, payload.RequestKey); err != nil {
			return IntentPoll{}, err
		}
		if payload.RetryAfter < 0 {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		return IntentPoll{State: StatePending, IntentID: payload.IntentID, RequestKey: payload.RequestKey,
			ExpiresAt: expires, RetryAfter: payload.RetryAfter, Gateway: &gateway}, nil
	case string(StateReady):
		var payload readyPayload
		if wlwire.StrictJSON(raw, &payload) != nil {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		if payload.State != payload.Status {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		expires, err := ParseTime(payload.ExpiresAt)
		if err != nil {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		challengeExpires, err := ParseTime(payload.StartChallenge.ExpiresAt)
		if err != nil {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		gateway, err := validateGateway(payload.Gateway)
		if err != nil {
			return IntentPoll{}, err
		}
		if err := validateIdentity(payload.IntentID, payload.RequestKey); err != nil {
			return IntentPoll{}, err
		}
		if payload.CredentialID == "" || payload.Bootstrap.CredentialID == "" || payload.Bootstrap.Secret == "" {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		if payload.StartChallenge.ChallengeID == "" || payload.StartChallenge.NonceB64 == "" {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		return IntentPoll{State: StateReady, IntentID: payload.IntentID, RequestKey: payload.RequestKey,
			ExpiresAt: expires, Gateway: &gateway, CredentialID: payload.CredentialID,
			Bootstrap: &payload.Bootstrap, StartChallenge: &StartChallenge{ChallengeID: payload.StartChallenge.ChallengeID,
				NonceB64: payload.StartChallenge.NonceB64, ExpiresAt: challengeExpires.Format(time.RFC3339)}}, nil
	case string(StateStarted):
		var payload startedPayload
		if wlwire.StrictJSON(raw, &payload) != nil {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		if payload.State != payload.Status {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		startedAt, err := ParseTime(payload.StartedAt)
		if err != nil {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		notAfter, err := ParseTime(payload.NotAfter)
		if err != nil || !notAfter.After(startedAt) {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		gateway, err := validateGateway(payload.Gateway)
		if err != nil {
			return IntentPoll{}, err
		}
		if err := validateIdentity(payload.IntentID, payload.RequestKey); err != nil {
			return IntentPoll{}, err
		}
		return IntentPoll{State: StateStarted, IntentID: payload.IntentID, RequestKey: payload.RequestKey,
			Gateway: &gateway, CredentialID: payload.CredentialID, StartedAt: startedAt, NotAfter: notAfter}, nil
	case string(StateFailed):
		var payload failedPayload
		if wlwire.StrictJSON(raw, &payload) != nil {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		if payload.State != payload.Status {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		if err := validateIdentity(payload.IntentID, payload.RequestKey); err != nil {
			return IntentPoll{}, err
		}
		if payload.Reason == "" || len(payload.Reason) > maxReason {
			return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
		}
		return IntentPoll{State: StateFailed, IntentID: payload.IntentID, RequestKey: payload.RequestKey,
			FailureReason: payload.Reason}, nil
	default:
		return IntentPoll{}, errors.New("ONBOARDING_INTENT_MALFORMED")
	}
}

func validateIdentity(intentID, requestKey string) error {
	if intentID == "" || len(intentID) > maxIntentID {
		return errors.New("ONBOARDING_INTENT_MALFORMED")
	}
	if requestKey == "" || len(requestKey) > maxRequestKey {
		return errors.New("ONBOARDING_INTENT_MALFORMED")
	}
	return nil
}

func validateGateway(gateway Gateway) (Gateway, error) {
	if gateway.NodeID == "" {
		return Gateway{}, errors.New("ONBOARDING_INTENT_MALFORMED")
	}
	ip := net.ParseIP(gateway.Endpoint.PeerIP)
	if ip == nil || ip.To4() == nil && ip.To16() == nil {
		return Gateway{}, errors.New("ONBOARDING_INTENT_MALFORMED")
	}
	if gateway.Endpoint.DTLSPort < 1 || gateway.Endpoint.DTLSPort > 65535 {
		return Gateway{}, errors.New("ONBOARDING_INTENT_MALFORMED")
	}
	spki := gateway.Endpoint.DTLSSPKISHA256
	if len(spki) != base64.RawURLEncoding.EncodedLen(32) {
		return Gateway{}, errors.New("ONBOARDING_INTENT_MALFORMED")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(spki)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != spki {
		return Gateway{}, errors.New("ONBOARDING_INTENT_MALFORMED")
	}
	return gateway, nil
}

func isLowerHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

// ParseTime accepts RFC3339 with a numeric offset as well as Z; contract rev3
// times are UTC Z, and "+00:00" must be accepted as equally valid.
func ParseTime(value string) (time.Time, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return time.Time{}, errors.New("ONBOARDING_TIME_INVALID")
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errors.New("ONBOARDING_TIME_INVALID")
	}
	return parsed.UTC(), nil
}

// APIError is one decoded error envelope with the exact backend code preserved
// (distinct codes are never collapsed into one name).
type APIError struct {
	HTTPStatus int
	Code       string
	Retryable  bool
	RetryAfter int
}

// Error returns the exact backend code.
func (e *APIError) Error() string { return e.Code }

// Is lets callers match a bounded sentinel via errors.Is without collapsing codes.
func (e *APIError) Is(target error) bool {
	return target != nil && e.Code == target.Error()
}

// Bounded error sentinels from the contract; the full exact code is kept in APIError.Code.
var (
	ErrIntentExpired = errors.New("ONBOARDING_INTENT_EXPIRED")
	ErrIntentRevoked = errors.New("ONBOARDING_INTENT_REVOKED")
	// ErrIntentConflict is the immutable-gateway-binding conflict: the same
	// request_key was already paired with another gateway_key. It matches the server
	// 409 code exactly and is also returned locally before any side effect.
	ErrIntentConflict      = errors.New("ONBOARDING_INTENT_CONFLICT")
	ErrStartConflict       = errors.New("ONBOARDING_START_CONFLICT")
	ErrIdempotencyConflict = errors.New("IDEMPOTENCY_CONFLICT")
	ErrUnitStarted         = errors.New("ONBOARDING_UNIT_STARTED")
	ErrChallengeExpired    = errors.New("CHALLENGE_EXPIRED")
	ErrProofInvalid        = errors.New("PROOF_INVALID")
	ErrReplayUnavailable   = errors.New("ONBOARDING_REPLAY_UNAVAILABLE")
	ErrUnknownOperation    = errors.New("EVIDENCE_UNKNOWN_OPERATION")
)

type errorPayload struct {
	// Public HTTP error envelope (auth_api._error_response).
	RequestID     string          `json:"request_id"`
	ServerTime    string          `json:"server_time"`
	SchemaVersion string          `json:"schema_version"`
	Status        string          `json:"status"`
	Code          string          `json:"code"`
	Retryable     bool            `json:"retryable"`
	RetryAfterMS  *int            `json:"retry_after_ms"`
	Details       json.RawMessage `json:"details"`
	// Start RPC error envelope (evidence_transport._error_response) carries
	// retry_after in whole seconds instead of retry_after_ms.
	RetryAfter *int `json:"retry_after"`
}

// DecodeError strictly decodes the bounded error envelope and preserves the code.
// Both the public HTTP shape (retry_after_ms) and the start RPC shape (retry_after)
// are accepted; RetryAfter is normalized to whole seconds.
func DecodeError(raw []byte, httpStatus int) (*APIError, error) {
	var payload errorPayload
	if wlwire.StrictJSON(raw, &payload) != nil {
		return nil, errors.New("ONBOARDING_ERROR_MALFORMED")
	}
	if payload.Status != "error" || payload.Code == "" || len(payload.Code) > maxErrorCode {
		return nil, errors.New("ONBOARDING_ERROR_MALFORMED")
	}
	retryAfter := 0
	switch {
	case payload.RetryAfter != nil:
		if *payload.RetryAfter < 0 {
			return nil, errors.New("ONBOARDING_ERROR_MALFORMED")
		}
		retryAfter = *payload.RetryAfter
	case payload.RetryAfterMS != nil:
		if *payload.RetryAfterMS < 0 {
			return nil, errors.New("ONBOARDING_ERROR_MALFORMED")
		}
		retryAfter = (*payload.RetryAfterMS + 999) / 1000
	}
	return &APIError{HTTPStatus: httpStatus, Code: payload.Code, Retryable: payload.Retryable,
		RetryAfter: retryAfter}, nil
}

// StartProof is the PoP proof carried by the explicit onboarding.start RPC.
type StartProof struct {
	Algorithm        string `json:"algorithm"`
	Environment      string `json:"environment"`
	RequestID        string `json:"request_id"`
	ChallengeID      string `json:"challenge_id"`
	NonceB64         string `json:"nonce_b64"`
	PayloadHash      string `json:"payload_hash"`
	SignedPayloadB64 string `json:"signed_payload_b64"`
	SignatureB64     string `json:"signature_b64"`
}

// StartBody is the exact RPC body (envelope body field, rev3 section 2).
type StartBody struct {
	V          int        `json:"v"`
	Op         string     `json:"op"`
	IntentID   string     `json:"intent_id"`
	RequestKey string     `json:"request_key"`
	Proof      StartProof `json:"proof"`
}

// EncodeStartBody validates and encodes the start RPC body.
func EncodeStartBody(body StartBody) ([]byte, error) {
	if err := validateStartBody(body); err != nil {
		return nil, err
	}
	return marshal(body)
}

// DecodeStartBody strictly decodes and validates a start RPC body.
func DecodeStartBody(raw []byte) (StartBody, error) {
	var body StartBody
	if wlwire.StrictJSON(raw, &body) != nil {
		return StartBody{}, errors.New("ONBOARDING_START_MALFORMED")
	}
	if err := validateStartBody(body); err != nil {
		return StartBody{}, err
	}
	return body, nil
}

func validateStartBody(body StartBody) error {
	if body.V != 1 || body.Op != StartOp {
		return errors.New("ONBOARDING_START_MALFORMED")
	}
	if body.IntentID == "" || len(body.IntentID) > maxIntentID {
		return errors.New("ONBOARDING_START_MALFORMED")
	}
	if body.RequestKey == "" || len(body.RequestKey) > maxRequestKey {
		return errors.New("ONBOARDING_START_MALFORMED")
	}
	proof := body.Proof
	if proof.Algorithm != "ES256" || proof.Environment == "" || proof.RequestID == "" ||
		proof.ChallengeID == "" || proof.NonceB64 == "" || proof.PayloadHash == "" ||
		proof.SignedPayloadB64 == "" || proof.SignatureB64 == "" {
		return errors.New("ONBOARDING_START_MALFORMED")
	}
	if len(proof.RequestID) != 32 || !isLowerHex(proof.RequestID) {
		return errors.New("ONBOARDING_START_MALFORMED")
	}
	if len(proof.ChallengeID) != 32 || !isLowerHex(proof.ChallengeID) {
		return errors.New("ONBOARDING_START_MALFORMED")
	}
	if len(proof.PayloadHash) != 64 || !isLowerHex(proof.PayloadHash) {
		return errors.New("ONBOARDING_START_MALFORMED")
	}
	return nil
}

// StartReply is the decoded transport-level start response.
type StartReply struct {
	IntentID     string
	CredentialID string
	StartedAt    time.Time
	NotAfter     time.Time
	Replay       bool
	// MessageHash and ConnectionIDHash are the evidence_transport diagnostics for
	// the admitted (or replayed) start; they are not secrets.
	MessageHash      string
	ConnectionIDHash string
}

type startReplyPayload struct {
	Status           string `json:"status"`
	State            string `json:"state"`
	IntentID         string `json:"intent_id"`
	CredentialID     string `json:"credential_id"`
	StartedAt        string `json:"started_at"`
	NotAfter         string `json:"not_after"`
	Replay           bool   `json:"replay"`
	MessageHash      string `json:"message_hash"`
	ConnectionIDHash string `json:"connection_id_hash"`
}

// DecodeStartReply decodes the ok or error start reply; error codes stay distinct.
func DecodeStartReply(raw []byte) (StartReply, *APIError, error) {
	var union map[string]any
	if wlwire.StrictJSON(raw, &union) != nil {
		return StartReply{}, nil, errors.New("ONBOARDING_START_REPLY_MALFORMED")
	}
	if union["status"] == "error" {
		apiError, err := DecodeError(raw, 0)
		if err != nil {
			return StartReply{}, nil, err
		}
		return StartReply{}, apiError, nil
	}
	var payload startReplyPayload
	if wlwire.StrictJSON(raw, &payload) != nil {
		return StartReply{}, nil, errors.New("ONBOARDING_START_REPLY_MALFORMED")
	}
	if payload.Status != "ok" || payload.State != string(StateStarted) {
		return StartReply{}, nil, errors.New("ONBOARDING_START_REPLY_MALFORMED")
	}
	if payload.IntentID == "" || len(payload.IntentID) > maxIntentID || payload.CredentialID == "" {
		return StartReply{}, nil, errors.New("ONBOARDING_START_REPLY_MALFORMED")
	}
	startedAt, err := ParseTime(payload.StartedAt)
	if err != nil {
		return StartReply{}, nil, errors.New("ONBOARDING_START_REPLY_MALFORMED")
	}
	notAfter, err := ParseTime(payload.NotAfter)
	if err != nil || !notAfter.After(startedAt) {
		return StartReply{}, nil, errors.New("ONBOARDING_START_REPLY_MALFORMED")
	}
	return StartReply{IntentID: payload.IntentID, CredentialID: payload.CredentialID,
		StartedAt: startedAt, NotAfter: notAfter, Replay: payload.Replay,
		MessageHash: payload.MessageHash, ConnectionIDHash: payload.ConnectionIDHash}, nil, nil
}

// ErrorText renders a bounded, secret-free diagnostic for one API error.
func ErrorText(err error) string {
	var apiError *APIError
	if errors.As(err, &apiError) {
		return fmt.Sprintf("HTTP %d %s", apiError.HTTPStatus, apiError.Code)
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func marshal(value any) ([]byte, error) {
	raw, err := strictMarshal(value)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > wlwire.ServiceMaxFrame {
		return nil, errors.New("ONBOARDING_BODY_MALFORMED")
	}
	return raw, nil
}

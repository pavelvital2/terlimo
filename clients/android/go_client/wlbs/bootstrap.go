package wlbs

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type RegisterPayload struct {
	Op             string `json:"op"`
	CredentialID   string `json:"credential_id"`
	PublicKeySPKI  string `json:"public_key_spki"`
	InstallationID string `json:"installation_id"`
	OS             string `json:"os"`
}
type CatalogPayload struct {
	Op             string `json:"op"`
	CredentialID   string `json:"credential_id"`
	RegistrationID string `json:"registration_id"`
	KnownRevision  string `json:"known_revision,omitempty"`
}
type RefreshPayload struct {
	Op                 string `json:"op"`
	CredentialID       string `json:"credential_id"`
	PublicKeySPKI      string `json:"public_key_spki"`
	InstallationID     string `json:"installation_id"`
	RegistrationID     string `json:"registration_id"`
	NodeID             string `json:"node_id"`
	GrantID            string `json:"grant_id"`
	ExpectedGeneration string `json:"expected_generation"`
	ExpectedLeaseSeq   string `json:"expected_lease_seq"`
}
type SyncAccessPayload struct {
	Op             string `json:"op"`
	CredentialID   string `json:"credential_id"`
	PublicKeySPKI  string `json:"public_key_spki"`
	InstallationID string `json:"installation_id"`
	RegistrationID string `json:"registration_id"`
}
type StatusPayload struct {
	Op                string `json:"op"`
	CredentialID      string `json:"credential_id"`
	OriginalRequestID string `json:"original_request_id"`
	PublicKeySPKI     string `json:"public_key_spki"`
	InstallationID    string `json:"installation_id"`
}
type BootstrapChallenge struct {
	V           int    `json:"v"`
	Op          string `json:"op"`
	Status      string `json:"status"`
	ChallengeID string `json:"challenge_id"`
	Nonce       string `json:"nonce"`
	ExpiresAt   string `json:"expires_at"`
	ServerTime  string `json:"server_time"`
}

// PendingOperation must be encrypted and durably saved before the first mutate.
// Resume uses this exact ID and these exact bytes with a fresh connection proof.
type PendingOperation struct {
	RequestID  string `json:"request_id"`
	Op         string `json:"op"`
	PayloadB64 string `json:"payload_b64"`
}
type PersistOperation func(context.Context, PendingOperation) error
type BootstrapClient struct {
	RPC           *RPC
	Signer        Signer
	CredentialID  string
	PublicKeySPKI []byte
	Persist       PersistOperation
}

func (b *BootstrapClient) Register(ctx context.Context) ([]byte, *PendingOperation, error) {
	finger, e := InstallationID(b.PublicKeySPKI)
	if e != nil {
		return nil, nil, e
	}
	p := RegisterPayload{"register", b.CredentialID, EncodeBinary(b.PublicKeySPKI), finger, "android"}
	return b.mutate(ctx, p)
}
func (b *BootstrapClient) Refresh(ctx context.Context, p RefreshPayload) ([]byte, *PendingOperation, error) {
	finger, e := InstallationID(b.PublicKeySPKI)
	if e != nil {
		return nil, nil, e
	}
	p.Op = "refresh_access"
	p.CredentialID = b.CredentialID
	p.PublicKeySPKI = EncodeBinary(b.PublicKeySPKI)
	p.InstallationID = finger
	return b.mutate(ctx, p)
}
func (b *BootstrapClient) SyncAccess(ctx context.Context, registration string) ([]byte, *PendingOperation, error) {
	finger, e := InstallationID(b.PublicKeySPKI)
	if e != nil {
		return nil, nil, e
	}
	p := SyncAccessPayload{"sync_access", b.CredentialID, EncodeBinary(b.PublicKeySPKI), finger, registration}
	return b.mutate(ctx, p)
}
func (b *BootstrapClient) mutate(ctx context.Context, p any) ([]byte, *PendingOperation, error) {
	if b.Persist == nil {
		return nil, nil, failure("PERSIST_REQUIRED")
	}
	raw, e := json.Marshal(p)
	if e != nil {
		return nil, nil, e
	}
	var meta struct {
		Op string `json:"op"`
	}
	if e = StrictJSON(raw, &meta); e != nil {
		return nil, nil, e
	}
	if e = ValidateBusinessPayload(raw, meta.Op); e != nil {
		return nil, nil, e
	}
	id, e := NewID()
	if e != nil {
		return nil, nil, e
	}
	pending := &PendingOperation{id.String(), meta.Op, EncodeBinary(raw)}
	if e = b.Persist(ctx, *pending); e != nil {
		return nil, nil, e
	}
	response, e := b.Resume(ctx, *pending)
	return response, pending, e
}
func (b *BootstrapClient) Resume(ctx context.Context, p PendingOperation) ([]byte, error) {
	if p.Op != "register" && p.Op != "refresh_access" && p.Op != "sync_access" {
		return nil, failure("BAD_MESSAGE")
	}
	id, e := ParseID(p.RequestID)
	if e != nil {
		return nil, e
	}
	payload, e := DecodeBinary(p.PayloadB64, -1)
	if e != nil {
		return nil, e
	}
	return b.SignedCall(ctx, id, p.Op, payload)
}
func (b *BootstrapClient) Catalog(ctx context.Context, registration, knownRevision string) ([]byte, error) {
	p := CatalogPayload{"catalog", b.CredentialID, registration, knownRevision}
	return b.query(ctx, p)
}
func (b *BootstrapClient) Status(ctx context.Context, p PendingOperation, _ string) ([]byte, error) {
	if _, e := ParseID(p.RequestID); e != nil {
		return nil, e
	}
	finger, e := InstallationID(b.PublicKeySPKI)
	if e != nil {
		return nil, e
	}
	payload := StatusPayload{"operation_status", b.CredentialID, p.RequestID, EncodeBinary(b.PublicKeySPKI), finger}
	return b.query(ctx, payload)
}
func (b *BootstrapClient) query(ctx context.Context, p any) ([]byte, error) {
	payload, e := json.Marshal(p)
	if e != nil {
		return nil, e
	}
	var meta struct {
		Op string `json:"op"`
	}
	_ = StrictJSON(payload, &meta)
	id, e := NewID()
	if e != nil {
		return nil, e
	}
	return b.SignedCall(ctx, id, meta.Op, payload)
}
func (b *BootstrapClient) SignedCall(ctx context.Context, id ID, op string, payload []byte) ([]byte, error) {
	if b.RPC == nil || b.Signer == nil {
		return nil, failure("KEY_UNAVAILABLE")
	}
	if e := ValidateBusinessPayload(payload, op); e != nil {
		return nil, e
	}
	var meta struct {
		CredentialID string `json:"credential_id"`
	}
	_ = StrictJSON(payload, &meta)
	if meta.CredentialID != b.CredentialID {
		return nil, failure("BAD_MESSAGE")
	}
	budget := MaxAttempts
	for budget > 0 {
		challengeStarted := time.Now()
		challengeID, e := NewID()
		if e != nil {
			return nil, e
		}
		request, _ := json.Marshal(struct {
			V  int    `json:"v"`
			Op string `json:"op"`
		}{1, "challenge"})
		raw, e := b.RPC.Call(ctx, challengeID, request)
		if e != nil {
			return nil, e
		}
		var c BootstrapChallenge
		if e = StrictJSON(raw, &c); e != nil {
			return nil, e
		}
		if c.V != 1 || c.Op != "challenge" || c.Status != "ok" {
			return nil, failure("BAD_MESSAGE")
		}
		expiry, e := UTC(c.ExpiresAt)
		if e != nil {
			return nil, e
		}
		now, e := UTC(c.ServerTime)
		if e != nil {
			return nil, e
		}
		if !expiry.After(now) || expiry.Sub(now) > 60*time.Second {
			return nil, failure("CHALLENGE_EXPIRED")
		}
		cid, e := DecodeBinary(c.ChallengeID, 16)
		if e != nil {
			return nil, e
		}
		nonce, e := DecodeBinary(c.Nonce, 32)
		if e != nil {
			return nil, e
		}
		t, e := BootstrapTranscript(id, cid, nonce, payload)
		if e != nil {
			return nil, e
		}
		signCtx, cancel := context.WithDeadline(ctx, challengeStarted.Add(expiry.Sub(now)))
		proof, e := b.Signer(signCtx, t)
		if e == nil {
			e = signCtx.Err()
		}
		cancel()
		if e != nil {
			return nil, e
		}
		if e = VerifyTranscript(b.PublicKeySPKI, t, proof); e != nil {
			return nil, e
		}
		body, _ := json.Marshal(ProofEnvelope{1, op, c.ChallengeID, EncodeBinary(payload), EncodeBinary(proof)})
		response, e := b.RPC.CallBudget(ctx, id, body, &budget)
		if e == nil {
			return response, nil
		}
		var wire *Error
		if errors.As(e, &wire) && wire.Code == "CHALLENGE_EXPIRED" && budget > 0 {
			continue
		}
		return nil, e
	}
	return nil, failure("RETRY_EXHAUSTED")
}

func ValidateBusinessPayload(raw []byte, op string) error {
	var meta struct {
		Op           string `json:"op"`
		CredentialID string `json:"credential_id"`
	}
	if StrictJSON(raw, &meta) != nil || meta.Op != op || meta.CredentialID == "" {
		return failure("BAD_MESSAGE")
	}
	switch op {
	case "register":
		var p RegisterPayload
		if StrictJSON(raw, &p) != nil || p.OS != "android" {
			return failure("BAD_MESSAGE")
		}
		key, e := DecodeBinary(p.PublicKeySPKI, -1)
		if e != nil {
			return e
		}
		finger, e := InstallationID(key)
		if e != nil || finger != p.InstallationID {
			return failure("BAD_MESSAGE")
		}
	case "catalog":
		var p CatalogPayload
		if StrictJSON(raw, &p) != nil || p.RegistrationID == "" {
			return failure("BAD_MESSAGE")
		}
		if p.KnownRevision != "" {
			if _, e := CompareDecimal(p.KnownRevision, "0"); e != nil {
				return e
			}
		}
	case "refresh_access":
		var p RefreshPayload
		if StrictJSON(raw, &p) != nil || p.RegistrationID == "" || p.NodeID == "" || p.GrantID == "" {
			return failure("BAD_MESSAGE")
		}
		key, e := DecodeBinary(p.PublicKeySPKI, -1)
		if e != nil {
			return e
		}
		finger, e := InstallationID(key)
		if e != nil || finger != p.InstallationID {
			return failure("BAD_MESSAGE")
		}
		if _, e := CompareDecimal(p.ExpectedGeneration, "0"); e != nil {
			return e
		}
		if _, e := CompareDecimal(p.ExpectedLeaseSeq, "0"); e != nil {
			return e
		}
	case "sync_access":
		var p SyncAccessPayload
		if StrictJSON(raw, &p) != nil || !exactObjectKeys(raw, "op", "credential_id", "public_key_spki", "installation_id", "registration_id") || p.RegistrationID == "" {
			return failure("BAD_MESSAGE")
		}
		key, e := DecodeBinary(p.PublicKeySPKI, -1)
		if e != nil {
			return e
		}
		finger, e := InstallationID(key)
		if e != nil || finger != p.InstallationID {
			return failure("BAD_MESSAGE")
		}
	case "operation_status":
		var p StatusPayload
		if StrictJSON(raw, &p) != nil || !exactObjectKeys(raw, "op", "credential_id", "public_key_spki", "installation_id", "original_request_id") {
			return failure("BAD_MESSAGE")
		}
		if _, e := ParseID(p.OriginalRequestID); e != nil {
			return e
		}
		key, e := DecodeBinary(p.PublicKeySPKI, -1)
		if e != nil {
			return e
		}
		finger, e := InstallationID(key)
		if e != nil || finger != p.InstallationID {
			return failure("BAD_MESSAGE")
		}
	default:
		return failure("UNSUPPORTED_OPERATION")
	}
	return nil
}

func exactObjectKeys(raw []byte, keys ...string) bool {
	var fields map[string]json.RawMessage
	if StrictJSON(raw, &fields) != nil || len(fields) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	return true
}

type OperationStatus struct {
	V       int      `json:"v"`
	Status  string   `json:"status"`
	Catalog *Catalog `json:"catalog,omitempty"`
	Code    string   `json:"code,omitempty"`
}

func (s *OperationStatus) Validate() error {
	if s.V != 1 {
		return failure("BAD_MESSAGE")
	}
	switch s.Status {
	case "complete":
		if s.Catalog == nil {
			return failure("BAD_CATALOG")
		}
	case "pending", "unknown":
		if s.Catalog != nil {
			return failure("BAD_MESSAGE")
		}
	case "failed":
		if s.Code == "" || s.Catalog != nil {
			return failure("BAD_MESSAGE")
		}
	default:
		return failure("BAD_MESSAGE")
	}
	return nil
}

// CatalogResponse is the agreed register/refresh success envelope. Validate the
// nested catalog with CatalogStore.Apply before committing or renewing access.
type CatalogResponse struct {
	V       int      `json:"v"`
	Status  string   `json:"status"`
	Catalog *Catalog `json:"catalog"`
}

func (r *CatalogResponse) Validate() error {
	if r.V != 1 || r.Status != "ok" || r.Catalog == nil {
		return failure("BAD_CATALOG")
	}
	return nil
}

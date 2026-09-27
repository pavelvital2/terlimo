package wlbs

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"time"
)

// Signer receives exact T, never a pre-hash. Android uses SHA256withECDSA.
type Signer func(context.Context, []byte) ([]byte, error)
type Exporter func(string, []byte, int) ([]byte, error)
type ProofEnvelope struct {
	V           int    `json:"v"`
	Op          string `json:"op"`
	ChallengeID string `json:"challenge_id"`
	PayloadB64  string `json:"payload_b64"`
	ProofB64    string `json:"proof_b64"`
}
type VPNIdentity struct {
	NodeID           string `json:"node_id"`
	GrantID          string `json:"grant_id"`
	RegistrationID   string `json:"registration_id"`
	Generation       string `json:"generation"`
	LeaseSeq         string `json:"lease_seq"`
	TransportSession string `json:"transport_session"`
	WorkerID         string `json:"worker_id"`
	Mode             string `json:"mode"`
}

func (i VPNIdentity) Validate() error {
	if i.NodeID == "" || i.GrantID == "" || i.RegistrationID == "" || i.TransportSession == "" || i.WorkerID == "" || (i.Mode != "getconf" && i.Mode != "data") {
		return failure("BAD_MESSAGE")
	}
	// Match v17's native GETCONF constraint without trimming signed bytes.
	if len(i.TransportSession) < 16 || len(i.TransportSession) > 64 {
		return failure("BAD_MESSAGE")
	}
	for _, c := range i.TransportSession {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return failure("BAD_MESSAGE")
		}
	}
	if _, e := CompareDecimal(i.WorkerID, "0"); e != nil {
		return e
	}
	if (i.Mode == "getconf") != (i.WorkerID == "0") {
		return failure("BAD_MESSAGE")
	}
	if _, e := CompareDecimal(i.Generation, "0"); e != nil {
		return e
	}
	_, e := CompareDecimal(i.LeaseSeq, "0")
	return e
}

// ValidateWorker also enforces the confirmed node policy. The transport must
// invoke it before auth because worker cap is not a field in the signed payload.
func (i VPNIdentity) ValidateWorker(maxWorkers int) error {
	if e := i.Validate(); e != nil {
		return e
	}
	if maxWorkers < 1 {
		return failure("BAD_MESSAGE")
	}
	cmp, e := CompareDecimal(i.WorkerID, strconv.Itoa(maxWorkers))
	if e != nil || cmp >= 0 {
		return failure("BAD_MESSAGE")
	}
	return nil
}

type VPNChallenge struct {
	V                  int    `json:"v"`
	Op                 string `json:"op"`
	Status             string `json:"status"`
	ChallengeID        string `json:"challenge_id"`
	Nonce              string `json:"nonce"`
	GrantID            string `json:"grant_id"`
	RegistrationID     string `json:"registration_id"`
	Generation         string `json:"generation"`
	LeaseSeq           string `json:"lease_seq"`
	NodeID             string `json:"node_id"`
	ChallengeExpiresAt string `json:"challenge_expires_at"`
	AccessExpiresAt    string `json:"access_expires_at"`
	AccessUnlimited    bool   `json:"-"`
	ServerTime         string `json:"server_time"`
}

func (v *VPNChallenge) UnmarshalJSON(data []byte) error {
	type alias VPNChallenge
	var decoded alias
	unlimited, err := decodeNullableString(data, "access_expires_at", &decoded)
	if err != nil {
		return err
	}
	decoded.AccessUnlimited = unlimited
	*v = VPNChallenge(decoded)
	return nil
}
func (v VPNChallenge) MarshalJSON() ([]byte, error) {
	type alias VPNChallenge
	return marshalNullableString(alias(v), "access_expires_at", v.AccessUnlimited)
}

type VPNOK struct {
	V           int    `json:"v"`
	Op          string `json:"op"`
	Status      string `json:"status"`
	ChallengeID string `json:"challenge_id"`
	VPNIdentity
	AccessExpiresAt string `json:"access_expires_at"`
	AccessUnlimited bool   `json:"-"`
	ServerTime      string `json:"server_time"`
}

func (v *VPNOK) UnmarshalJSON(data []byte) error {
	type alias VPNOK
	var decoded alias
	unlimited, err := decodeNullableString(data, "access_expires_at", &decoded)
	if err != nil {
		return err
	}
	decoded.AccessUnlimited = unlimited
	*v = VPNOK(decoded)
	return nil
}
func (v VPNOK) MarshalJSON() ([]byte, error) {
	type alias VPNOK
	return marshalNullableString(alias(v), "access_expires_at", v.AccessUnlimited)
}

// AuthenticateVPN authenticates ONE worker on an already pin-checked connection.
// On failure it closes the channel. Success does not imply Connected: native
// GETCONF/data probe and platform apply still have to succeed.
func AuthenticateVPN(ctx context.Context, conn net.Conn, exporter Exporter, signer Signer, identity VPNIdentity) (ok *VPNOK, err error) {
	if conn == nil {
		return nil, failure("TRANSPORT_CLOSED")
	}
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()
	if e := identity.Validate(); e != nil {
		return nil, e
	}
	if exporter == nil || signer == nil {
		return nil, failure("KEY_UNAVAILABLE")
	}
	material, err := exporter(ExporterLabel, nil, 32)
	if err != nil || len(material) != 32 {
		return nil, failure("TRUST_FAILED")
	}
	rpc := &RPC{Conn: conn}
	budget := MaxAttempts
	for budget > 0 {
		challengeStarted := time.Now()
		begin, e := NewID()
		if e != nil {
			return nil, e
		}
		raw, e := rpc.Call(ctx, begin, []byte(`{"v":1,"op":"VPN_AUTH_BEGIN"}`))
		if e != nil {
			return nil, e
		}
		var challenge VPNChallenge
		if e = StrictJSON(raw, &challenge); e != nil {
			return nil, e
		}
		if (challenge.V != 1 && challenge.V != 2) || challenge.Op != "VPN_CHALLENGE" || challenge.Status != "ok" || challenge.NodeID != identity.NodeID || challenge.GrantID != identity.GrantID || challenge.RegistrationID != identity.RegistrationID || (challenge.V == 1 && challenge.AccessUnlimited) || (challenge.V == 2 && !challenge.AccessUnlimited) {
			return nil, failure("BAD_MESSAGE")
		}
		if challenge.Generation != identity.Generation {
			return nil, failure("GRANT_REVOKED")
		}
		cmp, e := CompareDecimal(challenge.LeaseSeq, identity.LeaseSeq)
		if e != nil || cmp < 0 {
			return nil, failure("LEASE_CONFLICT")
		}
		identity.LeaseSeq = challenge.LeaseSeq
		server, e := UTC(challenge.ServerTime)
		if e != nil {
			return nil, e
		}
		expiry, e := UTC(challenge.ChallengeExpiresAt)
		if e != nil || !expiry.After(server) || expiry.Sub(server) > 15*time.Second {
			return nil, failure("CHALLENGE_EXPIRED")
		}
		if !challenge.AccessUnlimited {
			lease, leaseErr := UTC(challenge.AccessExpiresAt)
			if leaseErr != nil || !lease.After(server) {
				return nil, failure("LEASE_EXPIRED")
			}
		}
		cid, e := DecodeBinary(challenge.ChallengeID, 16)
		if e != nil {
			return nil, e
		}
		nonce, e := DecodeBinary(challenge.Nonce, 32)
		if e != nil {
			return nil, e
		}
		payload, e := json.Marshal(struct {
			Op string `json:"op"`
			VPNIdentity
		}{"vpn_auth", identity})
		if e != nil {
			return nil, e
		}
		t, e := VPNTranscript(material, cid, nonce, payload)
		if e != nil {
			return nil, e
		}
		signCtx, cancel := context.WithDeadline(ctx, challengeStarted.Add(expiry.Sub(server)))
		signature, signErr := signer(signCtx, t)
		if signErr == nil {
			signErr = signCtx.Err()
		}
		cancel()
		if signErr != nil {
			return nil, signErr
		}
		if len(signature) < 8 || len(signature) > 80 {
			return nil, failure("PROOF_INVALID")
		}
		body, _ := json.Marshal(ProofEnvelope{1, "VPN_AUTH", challenge.ChallengeID, EncodeBinary(payload), EncodeBinary(signature)})
		authID, e := NewID()
		if e != nil {
			return nil, e
		}
		raw, e = rpc.CallBudget(ctx, authID, body, &budget)
		if e != nil {
			var wire *Error
			if errors.As(e, &wire) && (wire.Code == "CHALLENGE_EXPIRED" || wire.Code == "LEASE_CONFLICT") && budget > 0 {
				continue
			}
			return nil, e
		}
		var result VPNOK
		if e = StrictJSON(raw, &result); e != nil {
			return nil, e
		}
		if result.V != challenge.V || result.Op != "VPN_AUTH_OK" || result.Status != "ok" || result.ChallengeID != challenge.ChallengeID || result.VPNIdentity != identity || result.AccessUnlimited != challenge.AccessUnlimited {
			return nil, failure("BAD_MESSAGE")
		}
		now, e := UTC(result.ServerTime)
		if e != nil {
			return nil, e
		}
		if !result.AccessUnlimited {
			end, endErr := UTC(result.AccessExpiresAt)
			if endErr != nil || !end.After(now) {
				return nil, failure("LEASE_EXPIRED")
			}
		}
		return &result, nil
	}
	return nil, failure("RETRY_EXHAUSTED")
}

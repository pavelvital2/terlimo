package wlbs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

func TestSyncAccessExactPayloadAndPersistBeforeSend(t *testing.T) {
	_, pub := localKey(t)
	finger, err := InstallationID(pub)
	if err != nil {
		t.Fatal(err)
	}
	persistErr := errors.New("synthetic stop before send")
	conn := &recordConn{}
	client := &BootstrapClient{
		RPC: &RPC{Conn: conn}, CredentialID: "fixture-only", PublicKeySPKI: pub,
		Signer: func(context.Context, []byte) ([]byte, error) {
			t.Fatal("sign called before persistence")
			return nil, nil
		},
		Persist: func(_ context.Context, pending PendingOperation) error {
			if pending.Op != "sync_access" {
				t.Fatal("wrong pending operation")
			}
			raw, decodeErr := DecodeBinary(pending.PayloadB64, -1)
			if decodeErr != nil || !exactBootstrapKeys(raw, "op", "credential_id", "public_key_spki", "installation_id", "registration_id") {
				t.Fatal("sync_access keys", decodeErr)
			}
			var got map[string]string
			if StrictJSON(raw, &got) != nil || !reflect.DeepEqual(got, map[string]string{
				"op": "sync_access", "credential_id": "fixture-only", "public_key_spki": EncodeBinary(pub),
				"installation_id": finger, "registration_id": "registration",
			}) {
				t.Fatal("sync_access payload changed")
			}
			return persistErr
		},
	}
	raw, pending, err := client.SyncAccess(context.Background(), "registration")
	if !errors.Is(err, persistErr) || raw != nil || pending != nil || len(conn.writes) != 0 {
		t.Fatal("persistence did not stop wire send", err)
	}
}

func TestLostSyncACKStatusSerializerMatchesStrictServerKeys(t *testing.T) {
	key, pub := localKey(t)
	var saved PendingOperation
	challenge := func(id ID, cid, nonce []byte) []byte {
		now := time.Now().UTC().Truncate(time.Second)
		raw, _ := json.Marshal(BootstrapChallenge{V: 1, Op: "challenge", Status: "ok", ChallengeID: EncodeBinary(cid), Nonce: EncodeBinary(nonce), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), ServerTime: now.Format(time.RFC3339)})
		frames, _ := Frames(id, true, raw)
		return frames[0]
	}
	syncConn := &recordConn{}
	syncCID, syncNonce := bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 32)
	syncConn.responder = func(record []byte) [][]byte {
		var id ID
		copy(id[:], record[8:24])
		var envelope ProofEnvelope
		if StrictJSON(record[HeaderSize:], &envelope) != nil {
			t.Fatal("invalid sync envelope")
		}
		if envelope.Op == "challenge" {
			return [][]byte{challenge(id, syncCID, syncNonce)}
		}
		if envelope.Op != "sync_access" || envelope.PayloadB64 != saved.PayloadB64 {
			t.Fatal("unexpected sync mutation")
		}
		return nil // server may have applied it; ACK is ambiguous/lost.
	}
	client := &BootstrapClient{
		RPC: &RPC{Conn: syncConn}, CredentialID: "fixture-only", PublicKeySPKI: pub,
		Signer:  func(_ context.Context, transcript []byte) ([]byte, error) { return sign(t, key, transcript), nil },
		Persist: func(_ context.Context, pending PendingOperation) error { saved = pending; return nil },
	}
	if raw, pending, err := client.SyncAccess(context.Background(), "registration"); !errors.Is(err, io.EOF) || raw != nil || pending == nil || *pending != saved {
		t.Fatal("lost sync ACK did not retain exact pending", err)
	}
	statusConn := &recordConn{}
	statusCID, statusNonce := bytes.Repeat([]byte{3}, 16), bytes.Repeat([]byte{4}, 32)
	statusConn.responder = func(record []byte) [][]byte {
		var id ID
		copy(id[:], record[8:24])
		var envelope ProofEnvelope
		if StrictJSON(record[HeaderSize:], &envelope) != nil {
			t.Fatal("invalid status envelope")
		}
		if envelope.Op == "challenge" {
			return [][]byte{challenge(id, statusCID, statusNonce)}
		}
		payload := unb64(t, envelope.PayloadB64)
		if envelope.Op != "operation_status" || !exactBootstrapKeys(payload, "op", "credential_id", "public_key_spki", "installation_id", "original_request_id") {
			t.Fatal("status payload does not match strict deployed parser")
		}
		var status StatusPayload
		if StrictJSON(payload, &status) != nil || status.OriginalRequestID != saved.RequestID || status.CredentialID != "fixture-only" {
			t.Fatal("status identity/request binding changed")
		}
		transcript, _ := BootstrapTranscript(id, statusCID, statusNonce, payload)
		if VerifyTranscript(pub, transcript, unb64(t, envelope.ProofB64)) != nil {
			t.Fatal("status transcript proof mismatch")
		}
		response := []byte(`{"v":1,"status":"pending"}`)
		frames, _ := Frames(id, true, response)
		return frames
	}
	client.RPC = &RPC{Conn: statusConn}
	raw, err := client.Status(context.Background(), saved, "must-not-be-serialized")
	if err != nil || string(raw) != `{"v":1,"status":"pending"}` {
		t.Fatal("strict status recovery failed", err)
	}
}

func TestSyncAccessValidationAndResumeOperation(t *testing.T) {
	_, pub := localKey(t)
	finger, _ := InstallationID(pub)
	valid, _ := json.Marshal(SyncAccessPayload{"sync_access", "credential", EncodeBinary(pub), finger, "registration"})
	if err := ValidateBusinessPayload(valid, "sync_access"); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []SyncAccessPayload{
		{"sync_access", "credential", EncodeBinary(pub), finger, ""},
		{"sync_access", "credential", EncodeBinary(pub), "wrong", "registration"},
	} {
		raw, _ := json.Marshal(payload)
		if ValidateBusinessPayload(raw, "sync_access") == nil {
			t.Fatal("invalid sync payload accepted")
		}
	}
	withExtra := append(valid[:len(valid)-1], []byte(`,"node_id":"forbidden"}`)...)
	if ValidateBusinessPayload(withExtra, "sync_access") == nil {
		t.Fatal("caller-selected node accepted")
	}
	p := PendingOperation{RequestID: "invalid", Op: "sync_access", PayloadB64: EncodeBinary(valid)}
	if _, err := (&BootstrapClient{}).Resume(context.Background(), p); err == nil {
		t.Fatal("invalid preserved request ID accepted")
	}
}

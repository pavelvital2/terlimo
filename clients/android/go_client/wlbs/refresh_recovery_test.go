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

func TestRefreshPersistsExactContractAndResumesLostResponse(t *testing.T) {
	key, pub := localKey(t)
	finger, err := InstallationID(pub)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"op": "refresh_access", "credential_id": "fixture-only",
		"public_key_spki": EncodeBinary(pub), "installation_id": finger,
		"registration_id": "registration", "node_id": "test-1", "grant_id": "grant",
		"expected_generation": "9007199254740993", "expected_lease_seq": "9007199254740995",
	}
	var saved PendingOperation
	persistCalls, challenges, mutations := 0, 0, 0
	var firstID ID
	var firstPayload, firstChallenge string
	catalog := testCatalog()
	wantResponse, err := json.Marshal(CatalogResponse{V: 1, Status: "ok", Catalog: &catalog})
	if err != nil {
		t.Fatal(err)
	}
	newConn := func(loseResponse bool) *recordConn {
		conn := &recordConn{}
		var cid, nonce []byte
		conn.responder = func(record []byte) [][]byte {
			if persistCalls != 1 || saved.RequestID == "" {
				t.Fatal("refresh sent before durable pending was saved")
			}
			var id ID
			copy(id[:], record[8:24])
			var envelope ProofEnvelope
			if err := StrictJSON(record[HeaderSize:], &envelope); err != nil {
				t.Fatal(err)
			}
			var response []byte
			switch envelope.Op {
			case "challenge":
				if !exactBootstrapKeys(record[HeaderSize:], "v", "op") {
					t.Fatal("challenge fields do not match deployed contract")
				}
				challenges++
				cid = bytes.Repeat([]byte{byte(challenges)}, 16)
				nonce = bytes.Repeat([]byte{byte(challenges + 16)}, 32)
				now := time.Now().UTC().Truncate(time.Second)
				response, err = json.Marshal(BootstrapChallenge{V: 1, Op: "challenge", Status: "ok", ChallengeID: EncodeBinary(cid), Nonce: EncodeBinary(nonce), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), ServerTime: now.Format(time.RFC3339)})
				if err != nil {
					t.Fatal(err)
				}
			case "refresh_access":
				mutations++
				if !exactBootstrapKeys(record[HeaderSize:], "v", "op", "challenge_id", "payload_b64", "proof_b64") {
					t.Fatal("refresh proof envelope fields changed")
				}
				payload := unb64(t, envelope.PayloadB64)
				var got map[string]string
				if err := StrictJSON(payload, &got); err != nil || !reflect.DeepEqual(got, want) {
					t.Fatal("refresh business fields do not match deployed contract", err)
				}
				pendingID, err := ParseID(saved.RequestID)
				if err != nil || id != pendingID || saved.Op != "refresh_access" || envelope.PayloadB64 != saved.PayloadB64 {
					t.Fatal("wire mutation differs from durable pending")
				}
				if envelope.ChallengeID != EncodeBinary(cid) {
					t.Fatal("proof does not use this connection's challenge")
				}
				transcript, err := BootstrapTranscript(id, cid, nonce, payload)
				if err != nil {
					t.Fatal(err)
				}
				if err := VerifyTranscript(pub, transcript, unb64(t, envelope.ProofB64)); err != nil {
					t.Fatal("refresh proof must sign SHA256 of the transcript once", err)
				}
				if mutations == 1 {
					firstID, firstPayload, firstChallenge = id, envelope.PayloadB64, envelope.ChallengeID
				} else if id != firstID || envelope.PayloadB64 != firstPayload || envelope.ChallengeID == firstChallenge {
					t.Fatal("resume must retain exact business ID/bytes with a fresh challenge")
				}
				if loseResponse {
					// The server received the complete mutation, but its reply was lost.
					return nil
				}
				response = wantResponse
			default:
				t.Fatalf("unexpected operation %q; refresh must never register", envelope.Op)
			}
			frames, err := Frames(id, true, response)
			if err != nil {
				t.Fatal(err)
			}
			return frames
		}
		return conn
	}
	client := &BootstrapClient{
		RPC: &RPC{Conn: newConn(true)}, CredentialID: "fixture-only", PublicKeySPKI: pub,
		Signer: func(_ context.Context, transcript []byte) ([]byte, error) { return sign(t, key, transcript), nil },
		Persist: func(_ context.Context, p PendingOperation) error {
			persistCalls++
			saved = p
			return nil
		},
	}
	raw, pending, err := client.Refresh(context.Background(), RefreshPayload{
		Op: "ignored", CredentialID: "ignored", RegistrationID: "registration", NodeID: "test-1", GrantID: "grant",
		ExpectedGeneration: want["expected_generation"], ExpectedLeaseSeq: want["expected_lease_seq"],
	})
	if !errors.Is(err, io.EOF) || raw != nil || pending == nil || *pending != saved {
		t.Fatal("lost response did not retain the exact persisted mutation", err)
	}
	client.RPC = &RPC{Conn: newConn(false)}
	client.Persist = func(context.Context, PendingOperation) error {
		t.Fatal("Resume must not persist a replacement operation")
		return nil
	}
	raw, err = client.Resume(context.Background(), saved)
	if err != nil || !bytes.Equal(raw, wantResponse) {
		t.Fatal("resume did not return the recovered response", err)
	}
	if persistCalls != 1 || mutations != 2 || challenges != 2 {
		t.Fatalf("unexpected calls: persist=%d mutations=%d challenges=%d", persistCalls, mutations, challenges)
	}
}

func TestRefreshPersistFailureSendsNothing(t *testing.T) {
	_, pub := localKey(t)
	conn := &recordConn{}
	persistErr := errors.New("synthetic persist failure")
	persistCalls, signCalls := 0, 0
	client := &BootstrapClient{
		RPC: &RPC{Conn: conn}, CredentialID: "fixture-only", PublicKeySPKI: pub,
		Signer: func(context.Context, []byte) ([]byte, error) {
			signCalls++
			return nil, errors.New("unexpected signer call")
		},
		Persist: func(_ context.Context, p PendingOperation) error {
			persistCalls++
			if p.Op != "refresh_access" || p.RequestID == "" || p.PayloadB64 == "" {
				t.Fatal("missing pending mutation")
			}
			return persistErr
		},
	}
	raw, pending, err := client.Refresh(context.Background(), RefreshPayload{
		RegistrationID: "registration", NodeID: "test-1", GrantID: "grant", ExpectedGeneration: "1", ExpectedLeaseSeq: "2",
	})
	if !errors.Is(err, persistErr) || raw != nil || pending != nil {
		t.Fatal("persist error was not returned before mutation", err)
	}
	if persistCalls != 1 || signCalls != 0 || len(conn.writes) != 0 {
		t.Fatal("failed persistence must prevent all signing and network sends")
	}
}

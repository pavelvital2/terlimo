package wlbs

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// Strict synthetic receiver exercises the production constructor, signer and
// RPC. It is NOT the deployed gateway, a DTLS test, or Android Keystore proof.
func TestCatalogExactRequestParity(t *testing.T) {
	raw, err := os.ReadFile("../../docs/fixtures/catalog_request_shape_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Challenge   string   `json:"challenge_json"`
		Full        string   `json:"full_payload_json"`
		Conditional string   `json:"conditional_payload_json"`
		Outer       []string `json:"outer_keys"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, revision, payload string
		lose                    bool
	}{
		{"full", "", fixture.Full, false},
		{"conditional", "9007199254740993", fixture.Conditional, false},
		{"full_read_eof", "", fixture.Full, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, pub := localKey(t)
			cid, nonce := bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 32)
			now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			catalog := testCatalog()
			response, err := json.Marshal(catalog)
			if err != nil {
				t.Fatal(err)
			}
			calls, signs := 0, 0
			var challengeRequest ID
			conn := &recordConn{}
			conn.responder = func(record []byte) [][]byte {
				calls++
				if len(record) < 32 || string(record[:4]) != "WLBS" || record[4] != 1 || record[5] != 0 || binary.BigEndian.Uint16(record[6:8]) != 32 || binary.BigEndian.Uint32(record[24:28]) != uint32(len(record)-32) || binary.BigEndian.Uint32(record[28:32]) != 0 {
					t.Fatal("not the expected complete single-frame request")
				}
				var id ID
				copy(id[:], record[8:24])
				if parsed, e := ParseID(id.String()); e != nil || parsed != id {
					t.Fatal("ID encoding")
				}
				body := record[32:]
				var reply []byte
				if calls == 1 {
					challengeRequest = id
					if string(body) != fixture.Challenge {
						t.Fatal("challenge bytes changed")
					}
					reply, err = json.Marshal(BootstrapChallenge{1, "challenge", "ok", EncodeBinary(cid), EncodeBinary(nonce), now.Add(time.Minute).Format(time.RFC3339), now.Format(time.RFC3339)})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					if calls != 2 || id == challengeRequest || !exactBootstrapKeys(body, fixture.Outer...) {
						t.Fatal("catalog envelope keys/ID/call count")
					}
					var env ProofEnvelope
					if StrictJSON(body, &env) != nil || env.V != 1 || env.Op != "catalog" || env.ChallengeID != EncodeBinary(cid) {
						t.Fatal("envelope types/binding")
					}
					payload := unb64(t, env.PayloadB64)
					if string(payload) != tc.payload {
						t.Fatal("constructor differs from exact fixture")
					}
					// Independent transcript assembly and standard ECDSA verification.
					tr := append([]byte("WLBS-POP-1\x00"), id[:]...)
					tr = append(tr, cid...)
					tr = append(tr, nonce...)
					h := sha256.Sum256(payload)
					tr = append(tr, h[:]...)
					digest := sha256.Sum256(tr)
					if !ecdsa.VerifyASN1(&key.PublicKey, digest[:], unb64(t, env.ProofB64)) {
						t.Fatal("signature/transcript mismatch")
					}
					if tc.lose {
						return nil
					}
					reply = response
				}
				frames, e := Frames(id, true, reply)
				if e != nil {
					t.Fatal(e)
				}
				return frames
			}
			client := &BootstrapClient{RPC: &RPC{Conn: conn}, CredentialID: "fixture-only", PublicKeySPKI: pub,
				Signer:  func(_ context.Context, tr []byte) ([]byte, error) { signs++; return sign(t, key, tr), nil },
				Persist: func(context.Context, PendingOperation) error { t.Fatal("catalog is not a mutation"); return nil },
			}
			got, err := client.Catalog(context.Background(), "registration", tc.revision)
			if tc.lose {
				if !errors.Is(err, io.EOF) || got != nil {
					t.Fatal("expected unchanged read EOF", err)
				}
			} else if err != nil || !bytes.Equal(got, response) {
				t.Fatal("catalog response", err)
			}
			if calls != 2 || signs != 1 || conn.closed {
				t.Fatal("unexpected retry, extra sign or own close")
			}
		})
	}
}

func TestCatalogStrictMockRejectsExtrasAndBadTypes(t *testing.T) {
	accept := func(raw string) bool {
		if !exactBootstrapKeys([]byte(raw), "op", "credential_id", "registration_id") {
			return false
		}
		var p CatalogPayload
		return StrictJSON([]byte(raw), &p) == nil && p.Op == "catalog" && p.CredentialID == "fixture-only" && p.RegistrationID == "registration"
	}
	for _, raw := range []string{
		`{"op":"catalog","credential_id":"fixture-only","registration_id":"registration","v":1}`,
		`{"op":"catalog","credential_id":"fixture-only","registration_id":"registration","public_key_spki":"extra"}`,
		`{"op":"catalog","credential_id":"fixture-only","registration_id":7}`,
		`{"op":"catalog","credential_id":"fixture-only"}`,
		`{"op":"refresh_access","credential_id":"fixture-only","registration_id":"registration"}`,
	} {
		if accept(raw) {
			t.Fatal("strict mock accepted invalid shape")
		}
	}
	if !accept(`{"op":"catalog","credential_id":"fixture-only","registration_id":"registration"}`) {
		t.Fatal("valid full shape rejected")
	}
}

package wlbs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

type signedSchemaFixture struct {
	VPNPayload    VPNIdentity        `json:"vpn_payload"`
	Catalog       Catalog            `json:"catalog"`
	Register      CatalogResponse    `json:"register_success"`
	Refresh       CatalogResponse    `json:"refresh_access_success"`
	Complete      OperationStatus    `json:"operation_status_complete"`
	Pending       OperationStatus    `json:"operation_status_pending"`
	Failed        OperationStatus    `json:"operation_status_failed"`
	Unknown       OperationStatus    `json:"operation_status_unknown"`
	Bootstrap     BootstrapChallenge `json:"bootstrap_challenge"`
	Challenge     VPNChallenge       `json:"vpn_challenge"`
	Auth          ProofEnvelope      `json:"vpn_auth"`
	OK            VPNOK              `json:"vpn_auth_ok"`
	StatusPayload json.RawMessage    `json:"status_request_payload"`
	NotModified   struct {
		V        int    `json:"v"`
		Status   string `json:"status"`
		Revision string `json:"revision"`
		Expires  string `json:"catalog_expires_at"`
		Checked  string `json:"checked_at"`
	} `json:"catalog_not_modified"`
	Signed struct {
		PublicKey       string   `json:"public_key_spki"`
		Exporter        string   `json:"synthetic_exporter_hex"`
		Transcript      string   `json:"transcript_hex"`
		SHA             string   `json:"sha256_transcript_hex"`
		BeginID         string   `json:"begin_request_id"`
		AuthID          string   `json:"auth_request_id"`
		BeginFrames     []string `json:"begin_frames_hex"`
		ChallengeFrames []string `json:"challenge_frames_hex"`
		AuthFrames      []string `json:"auth_frames_hex"`
		OKFrames        []string `json:"ok_frames_hex"`
	} `json:"signed_vpn"`
	Negative []struct {
		Field      string          `json:"field"`
		Value      json.RawMessage `json:"value"`
		Mode       string          `json:"mode"`
		MaxWorkers int             `json:"max_workers"`
	} `json:"negative_schema"`
}

func loadSignedSchema(t *testing.T) signedSchemaFixture {
	t.Helper()
	raw, err := os.ReadFile("../../docs/fixtures/wl_schema_signed_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != "edacbc6b0653a4bf63727fd9d8347405ece083b5bb578eaa9ab0f211a870c45d" {
		t.Fatal("fixture bytes changed")
	}
	var f signedSchemaFixture
	if err := StrictJSON(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSignedSchemaAcceptance(t *testing.T) {
	f := loadSignedSchema(t)
	// All schema/expiry assertions use the fixture clock, not the host clock.
	now, err := UTC(f.Bootstrap.ServerTime)
	if err != nil {
		t.Fatal(err)
	}
	if now.Format(time.RFC3339) != "2026-09-10T12:00:00Z" {
		t.Fatal("fixture clock changed")
	}
	for name, cat := range map[string]*Catalog{"catalog": &f.Catalog, "register": f.Register.Catalog, "refresh": f.Refresh.Catalog, "status_complete": f.Complete.Catalog} {
		t.Run(name, func(t *testing.T) {
			if err := cat.Validate("fixture-only", f.Catalog.RegistrationID, now); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cat, &f.Catalog) {
				t.Fatal("nested snapshot differs")
			}
		})
	}
	for _, r := range []CatalogResponse{f.Register, f.Refresh} {
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for name, s := range map[string]OperationStatus{"complete": f.Complete, "pending": f.Pending, "failed": f.Failed, "unknown": f.Unknown} {
		t.Run("operation_status_"+name, func(t *testing.T) {
			if err := s.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := f.VPNPayload.ValidateWorker(f.Catalog.Nodes[0].MaxWorkers); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBusinessPayload(f.StatusPayload, "operation_status"); err != nil {
		t.Fatal(err)
	}
	store := &CatalogStore{}
	if err := store.Apply(&f.Catalog, "fixture-only", f.Catalog.RegistrationID, now); err != nil {
		t.Fatal(err)
	}
	before := store.Snapshot()
	checked, err := UTC(f.NotModified.Checked)
	if err != nil || f.NotModified.V != 1 || f.NotModified.Status != "not_modified" || f.NotModified.Expires != before.CatalogExpiresAt {
		t.Fatal("not_modified envelope")
	}
	if err := store.NotModified(f.NotModified.Revision, checked); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, store.Snapshot()) {
		t.Fatal("not_modified extended snapshot")
	}
	expiry, _ := UTC(before.CatalogExpiresAt)
	wireCode(t, store.NotModified(f.NotModified.Revision, expiry), "CATALOG_EXPIRED")
	wireCode(t, before.Validate("fixture-only", before.RegistrationID, expiry), "BAD_CATALOG")
	if len(f.Negative) != 10 {
		t.Fatal("negative cases changed")
	}
	for index, n := range f.Negative {
		t.Run(fmt.Sprintf("reject_%02d_%s", index+1, n.Field), func(t *testing.T) {
			var value string
			if n.Field != "operation_status" {
				if err := StrictJSON(n.Value, &value); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			switch n.Field {
			case "worker_id":
				id := f.VPNPayload
				id.WorkerID = value
				if n.Mode != "" {
					id.Mode = n.Mode
				}
				cap := f.Catalog.Nodes[0].MaxWorkers
				if n.MaxWorkers != 0 {
					cap = n.MaxWorkers
				}
				err = id.ValidateWorker(cap)
			case "dtls_spki_sha256":
				cat := cloneCatalog(&f.Catalog)
				cat.Nodes[0].DTLSSPKISHA256 = value
				err = cat.Validate("fixture-only", cat.RegistrationID, now)
			case "original_request_id":
				var p StatusPayload
				if err := StrictJSON(f.StatusPayload, &p); err != nil {
					t.Fatal(err)
				}
				p.OriginalRequestID = value
				b, _ := json.Marshal(p)
				err = ValidateBusinessPayload(b, "operation_status")
			case "operation_status":
				var s OperationStatus
				if err := StrictJSON(n.Value, &s); err != nil {
					t.Fatal(err)
				}
				err = s.Validate()
			default:
				t.Fatal("unhandled case")
			}
			if err == nil {
				t.Fatal("invalid schema accepted")
			}
			t.Logf("REJECT: %v", err)
		})
	}
}

func TestSignedSchemaFramesAndVPNAuth(t *testing.T) {
	f := loadSignedSchema(t)
	now, _ := UTC(f.Challenge.ServerTime)
	for _, stage := range []struct {
		name, id string
		response bool
		frames   []string
		body     any
	}{
		{"begin", f.Signed.BeginID, false, f.Signed.BeginFrames, map[string]any{"v": 1, "op": "VPN_AUTH_BEGIN"}},
		{"challenge", f.Signed.BeginID, true, f.Signed.ChallengeFrames, f.Challenge},
		{"auth", f.Signed.AuthID, false, f.Signed.AuthFrames, f.Auth},
		{"ok", f.Signed.AuthID, true, f.Signed.OKFrames, f.OK},
	} {
		t.Run(stage.name, func(t *testing.T) {
			id, err := ParseID(stage.id)
			if err != nil {
				t.Fatal(err)
			}
			a := NewReassembler(id, stage.response)
			var body []byte
			var complete bool
			for _, raw := range stage.frames {
				body, complete, err = a.Add(unhex(t, raw), now)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !complete {
				t.Fatal("incomplete fixture")
			}
			var got, want any
			expected, _ := json.Marshal(stage.body)
			if StrictJSON(body, &got) != nil || StrictJSON(expected, &want) != nil || !reflect.DeepEqual(got, want) {
				t.Fatal("frame body differs from schema")
			}
			rebuilt, err := Frames(id, stage.response, body)
			if err != nil {
				t.Fatal(err)
			}
			for i, raw := range stage.frames {
				if !bytes.Equal(rebuilt[i], unhex(t, raw)) {
					t.Fatal("frame not byte exact")
				}
			}
		})
	}
	transcript, err := VPNTranscript(unhex(t, f.Signed.Exporter), unb64(t, f.Challenge.ChallengeID), unb64(t, f.Challenge.Nonce), unb64(t, f.Auth.PayloadB64))
	if err != nil || !bytes.Equal(transcript, unhex(t, f.Signed.Transcript)) {
		t.Fatal("T mismatch")
	}
	sum := sha256.Sum256(transcript)
	if hex.EncodeToString(sum[:]) != f.Signed.SHA {
		t.Fatal("digest mismatch")
	}
	if err := VerifyTranscript(unb64(t, f.Signed.PublicKey), transcript, unb64(t, f.Auth.ProofB64)); err != nil {
		t.Fatal(err)
	}
	wireCode(t, VerifyTranscript(unb64(t, f.Signed.PublicKey), sum[:], unb64(t, f.Auth.ProofB64)), "PROOF_INVALID")
	conn := &recordConn{}
	defer conn.Close()
	sends := 0
	conn.responder = func(frame []byte) [][]byte {
		sends++
		var id ID
		copy(id[:], frame[8:24])
		var meta struct {
			Op string `json:"op"`
		}
		if err := StrictJSON(frame[32:], &meta); err != nil {
			t.Fatal(err)
		}
		var response any
		switch meta.Op {
		case "VPN_AUTH_BEGIN":
			response = f.Challenge
		case "VPN_AUTH":
			var proof ProofEnvelope
			if err := StrictJSON(frame[32:], &proof); err != nil {
				t.Fatal(err)
			}
			if proof != f.Auth {
				t.Fatal("actual client AUTH differs from exact signed fixture")
			}
			response = f.OK
		default:
			t.Fatal("unexpected op")
		}
		body, _ := json.Marshal(response)
		frames, err := Frames(id, true, body)
		if err != nil {
			t.Fatal(err)
		}
		return frames
	}
	got, err := AuthenticateVPN(context.Background(), conn, func(label string, ctx []byte, size int) ([]byte, error) {
		if label != ExporterLabel || ctx != nil || size != 32 {
			t.Fatal("exporter contract")
		}
		return unhex(t, f.Signed.Exporter), nil
	}, func(ctx context.Context, actual []byte) ([]byte, error) {
		if !bytes.Equal(actual, transcript) {
			t.Fatal("actual signer T differs")
		}
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		return unb64(t, f.Auth.ProofB64), nil
	}, f.VPNPayload)
	if err != nil || got == nil || *got != f.OK || sends != 2 {
		t.Fatalf("actual AuthenticateVPN failed: %v sends=%d", err, sends)
	}
	// RPC IDs are generated by the client; the fixture response envelopes above
	// are correlated to these fresh IDs. Signed payload/T/proof are unchanged.
}

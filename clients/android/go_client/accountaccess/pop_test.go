package accountaccess

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// Contract vectors from terlimo-s1-contracts @ af06a80c (vectors/auth_vectors.json,
// TEST-ONLY synthetic key). This test proves the Go port matches the accepted bytes
// and signatures exactly; it does not claim any live schema PASS.

type authVector struct {
	Name                     string         `json:"name"`
	RequestID                string         `json:"request_id"`
	ChallengeID              string         `json:"challenge_id"`
	NonceB64                 string         `json:"nonce_b64"`
	Payload                  map[string]any `json:"payload"`
	PayloadCanonicalBytesHex string         `json:"payload_canonical_bytes_hex"`
	PayloadHash              string         `json:"payload_hash"`
	CanonicalMessageHex      string         `json:"canonical_message_hex"`
	SignedPayloadB64         string         `json:"signed_payload_b64"`
	SignatureB64             string         `json:"signature_b64"`
	Expected                 struct {
		Verify bool `json:"verify"`
	} `json:"expected"`
}

type authVectors struct {
	KnownTopLevel []string `json:"known_top_level"`
	TestKey       struct {
		PublicSPKIB64 string `json:"public_spki_b64"`
	} `json:"test_key"`
	Vectors             []authVector    `json:"vectors"`
	Changed             json.RawMessage `json:"changed_business_data"`
	Retry               json.RawMessage `json:"retry"`
	IdempotencyConflict json.RawMessage `json:"idempotency_conflict"`
	UnicodeNFC          json.RawMessage `json:"unicode_nfc"`
}

func loadVectors(t *testing.T) authVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/auth_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors authVectors
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	return vectors
}

func testPublicKey(t *testing.T, spkiB64 string) *ecdsa.PublicKey {
	t.Helper()
	der, err := B64URLDecodeStrict(spkiB64, -1)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		t.Fatal("not an EC public key")
	}
	return key
}

func TestPopVectorsCanonicalBytesDigestAndMessage(t *testing.T) {
	vectors := loadVectors(t)
	if len(vectors.KnownTopLevel) != len(KnownTopLevel) {
		t.Fatalf("known top-level size=%d want %d", len(vectors.KnownTopLevel), len(KnownTopLevel))
	}
	for _, name := range vectors.KnownTopLevel {
		if !KnownTopLevel[name] {
			t.Fatalf("missing known top-level field %q", name)
		}
	}
	for _, vector := range vectors.Vectors {
		canonical, err := CanonicalJSON(vector.Payload)
		if err != nil {
			t.Fatalf("%s canonical: %v", vector.Name, err)
		}
		if hex.EncodeToString(canonical) != vector.PayloadCanonicalBytesHex {
			t.Fatalf("%s canonical bytes mismatch\n got %s\nwant %s", vector.Name,
				hex.EncodeToString(canonical), vector.PayloadCanonicalBytesHex)
		}
		sum := sha256.Sum256(canonical)
		if hex.EncodeToString(sum[:]) != vector.PayloadHash {
			t.Fatalf("%s payload hash mismatch", vector.Name)
		}
		message, err := POPMessage(vector.RequestID, vector.ChallengeID, vector.NonceB64, canonical)
		if err != nil {
			t.Fatalf("%s message: %v", vector.Name, err)
		}
		if hex.EncodeToString(message) != vector.CanonicalMessageHex {
			t.Fatalf("%s message mismatch\n got %s\nwant %s", vector.Name,
				hex.EncodeToString(message), vector.CanonicalMessageHex)
		}
	}
}

func TestPopVectorsAcceptedSignaturesVerify(t *testing.T) {
	vectors := loadVectors(t)
	key := testPublicKey(t, vectors.TestKey.PublicSPKIB64)
	verified := 0
	for _, vector := range vectors.Vectors {
		if !vector.Expected.Verify {
			continue
		}
		raw, _, err := DecodeProofPayload(vector.SignedPayloadB64)
		if err != nil {
			t.Fatalf("%s decode: %v", vector.Name, err)
		}
		message, err := POPMessage(vector.RequestID, vector.ChallengeID, vector.NonceB64, raw)
		if err != nil {
			t.Fatal(err)
		}
		signature, err := B64URLDecodeStrict(vector.SignatureB64, -1)
		if err != nil {
			t.Fatalf("%s signature decode: %v", vector.Name, err)
		}
		digest := sha256.Sum256(message)
		if !ecdsa.VerifyASN1(key, digest[:], signature) {
			t.Fatalf("%s signature must verify", vector.Name)
		}
		verified++
	}
	if verified == 0 {
		t.Fatal("no accepted signature vectors")
	}
}

func TestPopVectorsWrongKeyAndTamperedPayloadRejected(t *testing.T) {
	vectors := loadVectors(t)
	for _, vector := range vectors.Vectors {
		switch vector.Name {
		case "wrong_key":
			raw, _, err := DecodeProofPayload(vector.SignedPayloadB64)
			if err != nil {
				t.Fatalf("wrong_key decode: %v", err)
			}
			message, _ := POPMessage(vector.RequestID, vector.ChallengeID, vector.NonceB64, raw)
			signature, _ := B64URLDecodeStrict(vector.SignatureB64, -1)
			digest := sha256.Sum256(message)
			if ecdsa.VerifyASN1(testPublicKey(t, vectors.TestKey.PublicSPKIB64), digest[:], signature) {
				t.Fatal("wrong_key signature must not verify with the accepted test key")
			}
		case "tampered_signed_payload":
			if _, _, err := DecodeProofPayload(vector.SignedPayloadB64); err == nil {
				t.Fatal("tampered signed payload must be rejected as non-canonical")
			}
		}
	}
}

func TestPopVectorsBusinessDigest(t *testing.T) {
	vectors := loadVectors(t)
	var changed struct {
		Before struct {
			Payload map[string]any `json:"payload"`
			Digest  string         `json:"digest"`
		} `json:"before"`
		After struct {
			Payload map[string]any `json:"payload"`
			Digest  string         `json:"digest"`
		} `json:"after"`
	}
	if err := json.Unmarshal(vectors.Changed, &changed); err != nil {
		t.Fatal(err)
	}
	before, err := BusinessDigest(changed.Before.Payload)
	if err != nil || before != changed.Before.Digest {
		t.Fatalf("before digest=%s want %s err=%v", before, changed.Before.Digest, err)
	}
	after, err := BusinessDigest(changed.After.Payload)
	if err != nil || after != changed.After.Digest {
		t.Fatalf("after digest=%s want %s err=%v", after, changed.After.Digest, err)
	}
	if before == after {
		t.Fatal("changed business data must change the digest")
	}

	var retry struct {
		Proofs []struct {
			Payload map[string]any `json:"payload"`
		} `json:"proofs"`
		BusinessDigests []string `json:"business_digests"`
	}
	if err := json.Unmarshal(vectors.Retry, &retry); err != nil {
		t.Fatal(err)
	}
	for index, proof := range retry.Proofs {
		digest, err := BusinessDigest(proof.Payload)
		if err != nil || digest != retry.BusinessDigests[index] {
			t.Fatalf("retry digest[%d]=%s want %s err=%v", index, digest, retry.BusinessDigests[index], err)
		}
	}
}

func TestPopVectorsUnicodeNFC(t *testing.T) {
	vectors := loadVectors(t)
	var unicode struct {
		Composed   string `json:"composed"`
		Decomposed string `json:"decomposed"`
	}
	if err := json.Unmarshal(vectors.UnicodeNFC, &unicode); err != nil {
		t.Fatal(err)
	}
	first, err := CanonicalJSON(map[string]any{"value": unicode.Composed})
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalJSON(map[string]any{"value": unicode.Decomposed})
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("NFC canonical forms differ: %s vs %s", first, second)
	}
}

func TestBusinessDigestScopeRules(t *testing.T) {
	base := map[string]any{
		"env": "test", "scope": "access:sync", "op": "N/A_SESSION_AUTH",
		"installation_id":  "b181d1ab5f72d9917771e06c480bfe86888161beeecba25050c8d8ce1f75a5e0",
		"catalog_revision": "7", "binding_revision": "1",
	}
	first, err := BusinessDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	base["ts"] = "2026-09-19T15:20:00Z"
	base["nonce"] = "Zm9v"
	base["request_id"] = "e0d986153b89eefb499763ee0d31ef1d"
	retry, err := BusinessDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	if first != retry {
		t.Fatal("freshness fields must not change the business digest")
	}
	base["catalog_revision"] = "9"
	changed, err := BusinessDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("changed action field must change the business digest")
	}
}

package accountaccess_test

// This is the exact attached shared server TEST vector, not a deployment key.
// Its HTTP example has a malformed 34-character request_id. Preserve/reject that
// original, then derive an explicitly corrected request_id-only synthetic HTTP
// envelope to join the decoder with the unchanged signed code and verifier.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"wg-turn-client/accountaccess"
	"wg-turn-client/servicechannel"
)

const serviceSeedServerVectorSHA = "1d0d90591d33831753814136daa0ddad58bc557710565bbee7c296194e5a4d96"

type serviceSeedVectorHTTP func(*http.Request) (*http.Response, error)

func (f serviceSeedVectorHTTP) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRecoveryServiceSeedSharedServerVectorCorrectedIDHTTPDecodeAndVerify(t *testing.T) {
	raw, err := os.ReadFile("../testdata/recovery-v1-server-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != serviceSeedServerVectorSHA {
		t.Fatal("shared server fixture changed; retain the exact attached bytes")
	}
	var fixture struct {
		TestOnly     bool                `json:"test_only"`
		PublicKeyB64 string              `json:"public_key_b64"`
		Seed         servicechannel.Seed `json:"seed"`
		RecoveryCode string              `json:"recovery_code"`
		HTTPExample  json.RawMessage     `json:"http_example"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || !fixture.TestOnly {
		t.Fatalf("shared TEST-only fixture invalid: %v", err)
	}
	key, err := servicechannel.DecodeRecoveryVerifyKey(fixture.PublicKeyB64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accountaccess.DecodeServiceSeed(fixture.HTTPExample); err == nil {
		t.Fatal("attached 34-character request_id was accepted by strict decoder")
	}
	const attachedInvalidRequestID = "00112233445566778899aabbccddeeff00"
	const correctedSyntheticRequestID = "00112233445566778899aabbccddeeff"
	correctedSyntheticHTTPEnvelope := bytes.Replace(fixture.HTTPExample,
		[]byte(attachedInvalidRequestID), []byte(correctedSyntheticRequestID), 1)
	if bytes.Equal(correctedSyntheticHTTPEnvelope, fixture.HTTPExample) {
		t.Fatal("attached request_id anomaly changed; review the pinned fixture")
	}
	calls := 0
	client := &accountaccess.Client{
		BaseURL: "https://synthetic.example.test/api/mobile/v1",
		Tokens:  accountaccess.StaticToken("synthetic-service-seed-token"),
		HTTP: serviceSeedVectorHTTP(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.Method != http.MethodGet || request.URL.Path != "/api/mobile/v1/service-seed" || request.URL.RawQuery != "" || request.Header.Get("Authorization") != "Bearer synthetic-service-seed-token" {
				t.Fatal("shared vector did not use the existing authorized GET transport")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(correctedSyntheticHTTPEnvelope))}, nil
		}),
	}
	response, apiError, err := client.GetServiceSeed(context.Background())
	if err != nil || apiError != nil {
		t.Fatalf("corrected-ID-only synthetic five-field envelope rejected: %v %v", err, apiError)
	}
	if calls != 1 || response.RequestID != correctedSyntheticRequestID || response.ServerTime != "2026-10-03T00:00:00Z" || response.SchemaVersion != "1.0" || response.RecoveryCode != fixture.RecoveryCode {
		t.Fatalf("HTTP decode changed server fields: calls=%d envelope=%+v", calls, response)
	}
	verified, err := servicechannel.VerifyRecoveryCode(response.RecoveryCode, key, "test")
	if err != nil {
		t.Fatalf("shared producer code failed Go verification after HTTP decode: %v", err)
	}
	actualSeed, err := json.Marshal(verified)
	if err != nil {
		t.Fatal(err)
	}
	expectedSeed, err := json.Marshal(fixture.Seed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actualSeed, expectedSeed) {
		t.Fatal("shared verified endpoint/pin/VK/revision differs from server fixture")
	}
}

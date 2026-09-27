package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"wg-turn-client/accountaccess"
)

type recordedRequest struct {
	method  string
	url     string
	headers http.Header
	body    []byte
}

type sequenceDoer struct {
	mu        sync.Mutex
	requests  []recordedRequest
	responses []func(recordedRequest) (*http.Response, error)
}

func (d *sequenceDoer) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	record := recordedRequest{method: req.Method, url: req.URL.String(), headers: req.Header.Clone(), body: body}
	d.mu.Lock()
	d.requests = append(d.requests, record)
	index := len(d.requests) - 1
	if index >= len(d.responses) {
		index = len(d.responses) - 1
	}
	handler := d.responses[index]
	d.mu.Unlock()
	return handler(record)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body))}
}

const challengeGolden = `{"request_id":"1f2e3d4c5b6a79880716253443526170","server_time":"2026-09-23T02:00:00Z",` +
	`"schema_version":"1","status":"ok","challenge_id":"00112233445566778899aabbccddeeff",` +
	`"nonce_b64":"bm9uY2U","purpose":"onboarding-start-intent","environment":"test",` +
	`"expires_at":"2026-09-23T02:10:00+00:00","single_use":true}`

func testClient(t *testing.T, doer accountaccess.Doer) *Client {
	t.Helper()
	client, err := NewClient(doer, accountaccess.StaticToken("session-bearer"), "https://api.example.test",
		Identity{Environment: EnvironmentTest, InstallationID: strings.Repeat("ab", 32),
			Signer: func(context.Context, []byte) ([]byte, error) { return make([]byte, 64), nil }})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return client
}

func TestClientIntentUsesServiceDoerAndExactPayload(t *testing.T) {
	doer := &sequenceDoer{responses: []func(recordedRequest) (*http.Response, error){
		func(recordedRequest) (*http.Response, error) { return jsonResponse(200, challengeGolden), nil },
		func(recordedRequest) (*http.Response, error) { return jsonResponse(200, readyGolden()), nil },
	}}
	client := testClient(t, doer)
	poll, apiError, err := client.Intent(context.Background(), testRequestKey, "")
	if err != nil || apiError != nil {
		t.Fatalf("intent: %v / %+v", err, apiError)
	}
	if poll.State != StateReady || poll.RequestKey != testRequestKey {
		t.Fatalf("poll: %+v", poll)
	}
	if len(doer.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(doer.requests))
	}
	challengeRequest := doer.requests[0]
	if challengeRequest.method != http.MethodPost ||
		challengeRequest.url != "https://api.example.test"+ChallengePath {
		t.Fatalf("challenge request: %s %s", challengeRequest.method, challengeRequest.url)
	}
	if challengeRequest.headers.Get("Authorization") != "Bearer session-bearer" {
		t.Fatalf("challenge must carry the session bearer")
	}
	var challengeBody map[string]any
	if err := json.Unmarshal(challengeRequest.body, &challengeBody); err != nil {
		t.Fatalf("challenge body: %v", err)
	}
	if challengeBody["purpose"] != IntentPurpose || challengeBody["environment"] != EnvironmentTest ||
		challengeBody["installation_fingerprint"] != strings.Repeat("ab", 32) {
		t.Fatalf("challenge body wrong: %v", challengeBody)
	}
	intentRequest := doer.requests[1]
	if intentRequest.url != "https://api.example.test"+IntentPath ||
		intentRequest.headers.Get("Authorization") != "Bearer session-bearer" ||
		intentRequest.headers.Get("Content-Type") != "application/json" {
		t.Fatalf("intent request wrong: %+v", intentRequest)
	}
	var envelope struct {
		Proof map[string]any `json:"proof"`
	}
	if err := json.Unmarshal(intentRequest.body, &envelope); err != nil {
		t.Fatalf("intent body: %v", err)
	}
	rawPayload, payload, err := accountaccess.DecodeProofPayload(strings.TrimSpace(envelope.Proof["signed_payload_b64"].(string)))
	if err != nil {
		t.Fatalf("signed payload must be canonical: %v", err)
	}
	if payload["scope"] != IntentScope || payload["op"] != IntentOp ||
		payload["request_key"] != testRequestKey || payload["installation_id"] != strings.Repeat("ab", 32) {
		t.Fatalf("signed payload fields wrong: %v", payload)
	}
	if envelope.Proof["request_id"] != payload["request_id"] ||
		envelope.Proof["nonce_b64"] != "bm9uY2U" || envelope.Proof["environment"] != EnvironmentTest {
		t.Fatalf("proof binding wrong: %v", envelope.Proof)
	}
	sum := sha256.Sum256(rawPayload)
	if envelope.Proof["payload_hash"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("payload_hash mismatch")
	}
	if envelope.Proof["algorithm"] != "ES256" {
		t.Fatalf("algorithm: %v", envelope.Proof["algorithm"])
	}
}

// TestClientIntentRefreshesChallengePerPoll proves the retry contract at the client
// seam: polling with the same request_key re-signs with a fresh challenge every time,
// so a retried poll never reuses the previous single-use challenge.
func TestClientIntentRefreshesChallengePerPoll(t *testing.T) {
	secondChallenge := strings.Replace(challengeGolden, `"challenge_id":"00112233445566778899aabbccddeeff"`,
		`"challenge_id":"ffeeddccbbaa99887766554433221100"`, 1)
	secondChallenge = strings.Replace(secondChallenge, `"nonce_b64":"bm9uY2U"`, `"nonce_b64":"bm9uY2Uy"`, 1)
	doer := &sequenceDoer{responses: []func(recordedRequest) (*http.Response, error){
		func(recordedRequest) (*http.Response, error) { return jsonResponse(200, challengeGolden), nil },
		func(recordedRequest) (*http.Response, error) {
			return jsonResponse(200, pendingGolden("2026-09-23T03:00:00Z")), nil
		},
		func(recordedRequest) (*http.Response, error) { return jsonResponse(200, secondChallenge), nil },
		func(recordedRequest) (*http.Response, error) { return jsonResponse(200, readyGolden()), nil },
	}}
	client := testClient(t, doer)
	first, apiError, err := client.Intent(context.Background(), testRequestKey, "")
	if err != nil || apiError != nil || first.State != StatePending {
		t.Fatalf("first poll: %+v / %+v / %v", first, apiError, err)
	}
	second, apiError, err := client.Intent(context.Background(), testRequestKey, "")
	if err != nil || apiError != nil || second.State != StateReady {
		t.Fatalf("second poll: %+v / %+v / %v", second, apiError, err)
	}
	if len(doer.requests) != 4 {
		t.Fatalf("requests = %d, want 4 (challenge + intent per poll)", len(doer.requests))
	}
	for index, want := range []string{ChallengePath, IntentPath, ChallengePath, IntentPath} {
		if doer.requests[index].url != "https://api.example.test"+want {
			t.Fatalf("request %d url = %s, want %s", index, doer.requests[index].url, want)
		}
	}
	wantChallenges := []string{"00112233445566778899aabbccddeeff", "ffeeddccbbaa99887766554433221100"}
	wantNonces := []string{"bm9uY2U", "bm9uY2Uy"}
	for pollIndex, requestIndex := range []int{1, 3} {
		var envelope struct {
			Proof map[string]any `json:"proof"`
		}
		if err := json.Unmarshal(doer.requests[requestIndex].body, &envelope); err != nil {
			t.Fatalf("poll %d intent body: %v", pollIndex, err)
		}
		if envelope.Proof["challenge_id"] != wantChallenges[pollIndex] ||
			envelope.Proof["nonce_b64"] != wantNonces[pollIndex] {
			t.Fatalf("poll %d must use its own fresh challenge: %v", pollIndex, envelope.Proof)
		}
		_, payload, err := accountaccess.DecodeProofPayload(strings.TrimSpace(envelope.Proof["signed_payload_b64"].(string)))
		if err != nil {
			t.Fatalf("poll %d signed payload: %v", pollIndex, err)
		}
		if payload["request_key"] != testRequestKey {
			t.Fatalf("poll %d must keep the request_key: %v", pollIndex, payload["request_key"])
		}
	}
}

func TestClientIntentMapsDistinctErrors(t *testing.T) {
	doer := &sequenceDoer{responses: []func(recordedRequest) (*http.Response, error){
		func(recordedRequest) (*http.Response, error) { return jsonResponse(200, challengeGolden), nil },
		func(recordedRequest) (*http.Response, error) {
			return jsonResponse(410, `{"status":"error","code":"ONBOARDING_INTENT_EXPIRED","retryable":false}`), nil
		},
	}}
	client := testClient(t, doer)
	_, apiError, err := client.Intent(context.Background(), testRequestKey, "")
	if err != nil || apiError == nil {
		t.Fatalf("intent error path: %v / %+v", err, apiError)
	}
	if !errors.Is(apiError, ErrIntentExpired) || apiError.HTTPStatus != 410 {
		t.Fatalf("expired code not preserved: %+v", apiError)
	}
	if errors.Is(apiError, ErrIntentRevoked) {
		t.Fatalf("distinct codes must not collapse")
	}
}

func TestClientIntentRejectsForeignChallenge(t *testing.T) {
	foreign := strings.Replace(challengeGolden, `"purpose":"onboarding-start-intent"`, `"purpose":"session"`, 1)
	doer := &sequenceDoer{responses: []func(recordedRequest) (*http.Response, error){
		func(recordedRequest) (*http.Response, error) { return jsonResponse(200, foreign), nil },
	}}
	client := testClient(t, doer)
	if _, _, err := client.Intent(context.Background(), testRequestKey, ""); err == nil {
		t.Fatalf("foreign-purpose challenge must be rejected")
	}
	if _, _, err := client.Intent(context.Background(), "", ""); err == nil {
		t.Fatalf("empty request_key must be rejected")
	}
}

func TestSignStartBodyRoundTripThroughCodec(t *testing.T) {
	identity := Identity{Environment: EnvironmentTest, InstallationID: strings.Repeat("ab", 32),
		Signer: func(context.Context, []byte) ([]byte, error) { return make([]byte, 64), nil }}
	challenge := StartChallenge{ChallengeID: "00112233445566778899aabbccddeeff", NonceB64: "bm9uY2U"}
	requestID, err := newRequestKey()
	if err != nil {
		t.Fatal(err)
	}
	body, err := SignStartBody(context.Background(), identity, testIntentID, testRequestKey, challenge, challenge.NonceB64, requestID)
	if err != nil {
		t.Fatalf("sign start: %v", err)
	}
	raw, err := EncodeStartBody(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeStartBody(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Op != StartOp || decoded.IntentID != testIntentID || decoded.Proof.PayloadHash == "" {
		t.Fatalf("start body wrong: %+v", decoded)
	}
	_, payload, err := accountaccess.DecodeProofPayload(decoded.Proof.SignedPayloadB64)
	if err != nil {
		t.Fatalf("start payload canonical: %v", err)
	}
	if payload["op"] != StartOp || payload["intent_id"] != testIntentID || payload["request_key"] != testRequestKey {
		t.Fatalf("start payload fields wrong: %v", payload)
	}
}

// TestClientIntentObservesOnlyTheFailingStage proves the diagnostic stage seam: exactly
// the failing stage name is reported once, a successful intent reports nothing, and the
// returned error identity is unchanged.
func TestClientIntentObservesOnlyTheFailingStage(t *testing.T) {
	okSigner := func(context.Context, []byte) ([]byte, error) { return make([]byte, 64), nil }
	failingSigner := func(context.Context, []byte) ([]byte, error) { return nil, errors.New("signer failed") }
	transportError := func(recordedRequest) (*http.Response, error) { return nil, errors.New("transport failed") }

	cases := []struct {
		name   string
		doer   accountaccess.Doer
		signer func(context.Context, []byte) ([]byte, error)
		want   string
	}{
		{"challenge", &sequenceDoer{responses: []func(recordedRequest) (*http.Response, error){transportError}}, okSigner, "challenge_transport"},
		{"sign", &sequenceDoer{responses: []func(recordedRequest) (*http.Response, error){
			func(recordedRequest) (*http.Response, error) { return jsonResponse(200, challengeGolden), nil }}}, failingSigner, "sign"},
		{"post", &sequenceDoer{responses: []func(recordedRequest) (*http.Response, error){
			func(recordedRequest) (*http.Response, error) { return jsonResponse(200, challengeGolden), nil },
			transportError}}, okSigner, "post"},
		{"decode", &sequenceDoer{responses: []func(recordedRequest) (*http.Response, error){
			func(recordedRequest) (*http.Response, error) { return jsonResponse(200, challengeGolden), nil },
			func(recordedRequest) (*http.Response, error) { return jsonResponse(200, "not-json"), nil }}}, okSigner, "decode"},
	}
	for _, tc := range cases {
		client, err := NewClient(tc.doer, accountaccess.StaticToken("session-bearer"), "https://api.example.test",
			Identity{Environment: EnvironmentTest, InstallationID: strings.Repeat("ab", 32), Signer: tc.signer})
		if err != nil {
			t.Fatalf("%s: client: %v", tc.name, err)
		}
		var observed []string
		client.ObserveStage = func(stage string) { observed = append(observed, stage) }
		if _, _, err := client.Intent(context.Background(), testRequestKey, ""); err == nil {
			t.Fatalf("%s: expected a stage failure", tc.name)
		}
		if len(observed) != 1 || observed[0] != tc.want {
			t.Fatalf("%s: observed stages = %v, want [%s]", tc.name, observed, tc.want)
		}
	}

	success := &sequenceDoer{responses: []func(recordedRequest) (*http.Response, error){
		func(recordedRequest) (*http.Response, error) { return jsonResponse(200, challengeGolden), nil },
		func(recordedRequest) (*http.Response, error) { return jsonResponse(200, readyGolden()), nil }}}
	client, err := NewClient(success, accountaccess.StaticToken("session-bearer"), "https://api.example.test",
		Identity{Environment: EnvironmentTest, InstallationID: strings.Repeat("ab", 32), Signer: okSigner})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	var observed []string
	client.ObserveStage = func(stage string) { observed = append(observed, stage) }
	if _, _, err := client.Intent(context.Background(), testRequestKey, ""); err != nil {
		t.Fatalf("success intent: %v", err)
	}
	if len(observed) != 0 {
		t.Fatalf("success must observe no failing stage: %v", observed)
	}
}

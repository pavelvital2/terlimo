package onboarding

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// zzNonce43 is a canonical 43-char base64url nonce (32 bytes), matching the accepted
// server contract shape.
var zzNonce43 = base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("n", 32)))

// zzExactServerChallenge is the exact 10-field server envelope from the accepted
// builder (schema 1.0, RFC3339Z times, 32-hex ids, 43-char base64url nonce).
const zzExactServerChallenge = `{"request_id":"1f2e3d4c5b6a79880716253443526170",` +
	`"server_time":"2030-01-01T00:00:00Z","schema_version":"1.0","status":"ok",` +
	`"challenge_id":"00112233445566778899aabbccddeeff","nonce_b64":"NONCE","purpose":"onboarding-start-intent",` +
	`"environment":"test","expires_at":"2030-01-01T00:10:00Z","single_use":true}`

func TestChallengeExactServerEnvelopeAccepted(t *testing.T) {
	body := strings.Replace(zzExactServerChallenge, "NONCE", zzNonce43, 1)
	d := &sequenceDoer{responses: []func(recordedRequest) (*http.Response, error){
		func(recordedRequest) (*http.Response, error) { return jsonResponse(200, body), nil }}}
	client := testClient(t, d)
	var observed []string
	client.ObserveStage = func(stage string) { observed = append(observed, stage) }
	challenge, err := client.challenge(context.Background(), IntentPurpose)
	if err != nil {
		t.Fatalf("exact server envelope rejected: %v", err)
	}
	if challenge.ChallengeID != "00112233445566778899aabbccddeeff" || len(challenge.NonceB64) != 43 {
		t.Fatalf("challenge shape wrong: %+v", challenge)
	}
	if len(observed) != 0 {
		t.Fatalf("accepted envelope must observe no stage: %v", observed)
	}
}

func TestChallengeFailureClasses(t *testing.T) {
	transportErr := func(recordedRequest) (*http.Response, error) { return nil, errors.New("secret-transport-token") }
	cases := []struct {
		name string
		resp func(recordedRequest) (*http.Response, error)
		want string
	}{
		{"transport", transportErr, "challenge_transport"},
		{"status_4xx", func(recordedRequest) (*http.Response, error) { return jsonResponse(403, "denied"), nil }, "challenge_status_4xx"},
		{"status_5xx", func(recordedRequest) (*http.Response, error) { return jsonResponse(500, "oops"), nil }, "challenge_status_5xx"},
		{"status_other", func(recordedRequest) (*http.Response, error) { return jsonResponse(302, ""), nil }, "challenge_status_other"},
		{"decode", func(recordedRequest) (*http.Response, error) { return jsonResponse(200, "not-json"), nil }, "challenge_decode"},
		{"semantic_purpose", func(recordedRequest) (*http.Response, error) {
			return jsonResponse(200, strings.Replace(strings.Replace(zzExactServerChallenge, "NONCE", zzNonce43, 1), "onboarding-start-intent", "other-purpose", 1)), nil
		}, "challenge_semantic"},
		{"semantic_time", func(recordedRequest) (*http.Response, error) {
			return jsonResponse(200, strings.Replace(strings.Replace(zzExactServerChallenge, "NONCE", zzNonce43, 1), "2030-01-01T00:10:00Z", "not-a-timestamp", 1)), nil
		}, "challenge_semantic"},
	}
	for _, tc := range cases {
		client := testClient(t, &sequenceDoer{responses: []func(recordedRequest) (*http.Response, error){tc.resp}})
		var observed []string
		client.ObserveStage = func(stage string) { observed = append(observed, stage) }
		if _, err := client.challenge(context.Background(), IntentPurpose); err == nil {
			t.Fatalf("%s: expected failure", tc.name)
		}
		if len(observed) != 1 || observed[0] != tc.want {
			t.Fatalf("%s: observed %v, want [%s]", tc.name, observed, tc.want)
		}
	}
}

func TestChallengeStageEmissionHasNoSecretText(t *testing.T) {
	client := testClient(t, &sequenceDoer{responses: []func(recordedRequest) (*http.Response, error){
		func(recordedRequest) (*http.Response, error) { return nil, errors.New("Bearer sekrit-value https://x/?token=abc") }}})
	var observed []string
	client.ObserveStage = func(stage string) { observed = append(observed, stage) }
	if _, err := client.challenge(context.Background(), IntentPurpose); err == nil {
		t.Fatal("expected failure")
	}
	if len(observed) != 1 || observed[0] != "challenge_transport" {
		t.Fatalf("observed %v", observed)
	}
	if strings.Contains(strings.Join(observed, " "), "sekrit") {
		t.Fatalf("secret text leaked into stage: %v", observed)
	}
}

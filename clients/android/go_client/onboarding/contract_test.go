package onboarding

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testIntentID   = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	testRequestKey = "aabbccddeeff00112233445566778899"
	testSPKI       = "PAyFy4YlbMsIfA4GfHvX_r8-HUWKVuj9PUeJ4pEtAjo"
)

func pendingGolden(expiresAt string) string {
	return `{"status":"pending","state":"pending","intent_id":"` + testIntentID + `",` +
		`"request_key":"` + testRequestKey + `","expires_at":"` + expiresAt + `","retry_after":5,` +
		`"gateway":{"node_id":"node-4397f3b","endpoint":{"peer_ip":"203.0.113.7","dtls_port":56002,` +
		`"dtls_spki_sha256":"` + testSPKI + `"}}}`
}

func readyGolden() string {
	return `{"status":"ready","state":"ready","intent_id":"` + testIntentID + `",` +
		`"request_key":"` + testRequestKey + `","expires_at":"2026-09-23T02:40:00Z",` +
		`"gateway":{"node_id":"node-4397f3b","endpoint":{"peer_ip":"203.0.113.7","dtls_port":56002,` +
		`"dtls_spki_sha256":"` + testSPKI + `"}},"credential_id":"cred-1",` +
		`"bootstrap":{"credential_id":"cred-1","secret":"c2VjcmV0"},` +
		`"start_challenge":{"challenge_id":"00112233445566778899aabbccddeeff",` +
		`"nonce_b64":"bm9uY2U","expires_at":"2026-09-23T02:10:00Z"}}`
}

func startedGolden() string {
	return `{"status":"started","state":"started","intent_id":"` + testIntentID + `",` +
		`"request_key":"` + testRequestKey + `",` +
		`"gateway":{"node_id":"node-4397f3b","endpoint":{"peer_ip":"203.0.113.7","dtls_port":56002,` +
		`"dtls_spki_sha256":"` + testSPKI + `"}},` +
		`"started_at":"2026-09-23T02:00:00Z","not_after":"2026-09-23T03:00:00Z"}`
}

func failedGolden() string {
	return `{"status":"failed","state":"failed","intent_id":"` + testIntentID + `",` +
		`"request_key":"` + testRequestKey + `","reason":"GATEWAY_UNAVAILABLE"}`
}

func TestDecodePollStates(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		state  State
		assert func(t *testing.T, poll IntentPoll)
	}{
		{"pending", pendingGolden("2026-09-23T03:00:00Z"), StatePending, func(t *testing.T, poll IntentPoll) {
			if poll.IntentID != testIntentID || poll.RequestKey != testRequestKey || poll.RetryAfter != 5 {
				t.Fatalf("pending correlation fields wrong: %+v", poll)
			}
			if poll.Gateway == nil || poll.Gateway.NodeID != "node-4397f3b" ||
				poll.Gateway.Endpoint.PeerIP != "203.0.113.7" || poll.Gateway.Endpoint.DTLSPort != 56002 ||
				poll.Gateway.Endpoint.DTLSSPKISHA256 != testSPKI {
				t.Fatalf("pending gateway wrong: %+v", poll.Gateway)
			}
			if !poll.ExpiresAt.Equal(time.Date(2026, 9, 23, 3, 0, 0, 0, time.UTC)) {
				t.Fatalf("pending expires_at wrong: %s", poll.ExpiresAt)
			}
		}},
		{"ready", readyGolden(), StateReady, func(t *testing.T, poll IntentPoll) {
			if poll.CredentialID != "cred-1" || poll.Bootstrap == nil || poll.Bootstrap.Secret != "c2VjcmV0" {
				t.Fatalf("ready bootstrap wrong: %+v", poll)
			}
			if poll.StartChallenge == nil || poll.StartChallenge.ChallengeID != "00112233445566778899aabbccddeeff" ||
				poll.StartChallenge.NonceB64 != "bm9uY2U" {
				t.Fatalf("ready challenge wrong: %+v", poll.StartChallenge)
			}
		}},
		{"started", startedGolden(), StateStarted, func(t *testing.T, poll IntentPoll) {
			if !poll.NotAfter.Equal(poll.StartedAt.Add(time.Hour)) {
				t.Fatalf("started window wrong: %s..%s", poll.StartedAt, poll.NotAfter)
			}
			if poll.Bootstrap != nil {
				t.Fatalf("started must never carry a bootstrap secret")
			}
		}},
		{"failed", failedGolden(), StateFailed, func(t *testing.T, poll IntentPoll) {
			if poll.FailureReason != "GATEWAY_UNAVAILABLE" {
				t.Fatalf("failed reason wrong: %+v", poll)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poll, err := DecodePoll([]byte(tc.raw))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if poll.State != tc.state {
				t.Fatalf("state = %s, want %s", poll.State, tc.state)
			}
			tc.assert(t, poll)
		})
	}
}

func TestDecodePollAcceptsOffsetZero(t *testing.T) {
	poll, err := DecodePoll([]byte(pendingGolden("2026-09-23T03:00:00+00:00")))
	if err != nil {
		t.Fatalf("RFC3339 +00:00 must be accepted: %v", err)
	}
	if !poll.ExpiresAt.Equal(time.Date(2026, 9, 23, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("offset +00:00 not normalized: %s", poll.ExpiresAt)
	}
}

func TestDecodePollRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"unknown field":     strings.Replace(readyGolden(), `"credential_id":"cred-1"`, `"credential_id":"cred-1","extra":true`, 1),
		"status mismatch":   strings.Replace(pendingGolden("2026-09-23T03:00:00Z"), `"state":"pending"`, `"state":"ready"`, 1),
		"missing request":   `{"status":"failed","state":"failed","intent_id":"x","reason":"r"}`,
		"bad time":          pendingGolden("2026-09-23 03:00:00"),
		"bad retry_after":   strings.Replace(pendingGolden("2026-09-23T03:00:00Z"), `"retry_after":5`, `"retry_after":-1`, 1),
		"bad gateway port":  strings.Replace(pendingGolden("2026-09-23T03:00:00Z"), `"dtls_port":56002`, `"dtls_port":0`, 1),
		"empty secret":      strings.Replace(readyGolden(), `"secret":"c2VjcmV0"`, `"secret":""`, 1),
		"bad spki sha":      strings.Replace(pendingGolden("2026-09-23T03:00:00Z"), testSPKI, "ABCDEF", 1),
		"padded spki":       strings.Replace(pendingGolden("2026-09-23T03:00:00Z"), testSPKI, testSPKI+"=", 1),
		"legacy hex spki":   strings.Replace(pendingGolden("2026-09-23T03:00:00Z"), testSPKI, strings.Repeat("a", 64), 1),
		"invalid spki char": strings.Replace(pendingGolden("2026-09-23T03:00:00Z"), testSPKI, "*"+testSPKI[1:], 1),
		"noncanonical spki": strings.Replace(pendingGolden("2026-09-23T03:00:00Z"), testSPKI, testSPKI[:42]+"p", 1),
		"started no window": strings.Replace(startedGolden(), `"not_after":"2026-09-23T03:00:00Z"`, `"not_after":"2026-09-23T02:00:00Z"`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodePoll([]byte(raw)); err == nil {
				t.Fatalf("malformed poll accepted: %s", raw)
			}
		})
	}
}

func TestDecodeErrorKeepsDistinctCodes(t *testing.T) {
	cases := []struct {
		code       string
		httpStatus int
		sentinel   error
	}{
		{"ONBOARDING_INTENT_EXPIRED", 410, ErrIntentExpired},
		{"ONBOARDING_INTENT_REVOKED", 403, ErrIntentRevoked},
		{"ONBOARDING_START_CONFLICT", 409, ErrStartConflict},
		{"IDEMPOTENCY_CONFLICT", 409, ErrIdempotencyConflict},
		{"ONBOARDING_UNIT_STARTED", 409, ErrUnitStarted},
		{"CHALLENGE_EXPIRED", 401, ErrChallengeExpired},
		{"PROOF_INVALID", 401, ErrProofInvalid},
		{"ONBOARDING_REPLAY_UNAVAILABLE", 409, ErrReplayUnavailable},
		{"EVIDENCE_UNKNOWN_OPERATION", 400, ErrUnknownOperation},
	}
	for _, tc := range cases {
		raw := `{"status":"error","code":"` + tc.code + `","retryable":false}`
		apiError, err := DecodeError([]byte(raw), tc.httpStatus)
		if err != nil {
			t.Fatalf("%s: %v", tc.code, err)
		}
		if apiError.Code != tc.code || apiError.HTTPStatus != tc.httpStatus {
			t.Fatalf("code/status not preserved: %+v", apiError)
		}
		if !errors.Is(apiError, tc.sentinel) {
			t.Fatalf("%s does not match its sentinel", tc.code)
		}
	}
	startConflict, _ := DecodeError([]byte(`{"status":"error","code":"ONBOARDING_START_CONFLICT","retryable":false}`), 409)
	if errors.Is(startConflict, ErrIdempotencyConflict) {
		t.Fatalf("409 codes must not collapse into one name")
	}
	if _, err := DecodeError([]byte(`{"status":"error","code":"","retryable":false}`), 409); err == nil {
		t.Fatalf("empty error code must be rejected")
	}
	if _, err := DecodeError([]byte(`{"status":"error","code":"X","retryable":false,"extra":1}`), 409); err == nil {
		t.Fatalf("unknown error field must be rejected")
	}
}

func TestStartBodyRoundTrip(t *testing.T) {
	challenge := StartChallenge{ChallengeID: "00112233445566778899aabbccddeeff", NonceB64: "bm9uY2U"}
	requestID, err := newRequestKey()
	if err != nil {
		t.Fatal(err)
	}
	body := StartBody{
		V: 1, Op: StartOp, IntentID: testIntentID, RequestKey: testRequestKey,
		Proof: StartProof{
			Algorithm: "ES256", Environment: EnvironmentTest, RequestID: requestID,
			ChallengeID: challenge.ChallengeID, NonceB64: challenge.NonceB64,
			PayloadHash: strings.Repeat("ab", 32), SignedPayloadB64: "cGF5bG9hZA", SignatureB64: "c2ln",
		},
	}
	raw, err := EncodeStartBody(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeStartBody(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.IntentID != body.IntentID || decoded.RequestKey != body.RequestKey || decoded.Proof != body.Proof {
		t.Fatalf("round trip mismatch: %+v", decoded)
	}
	malformed := []string{
		strings.Replace(string(raw), `"v":1`, `"v":2`, 1),
		strings.Replace(string(raw), `"op":"onboarding.start"`, `"op":"onboarding.intent"`, 1),
		strings.Replace(string(raw), requestID, "zz", 1),
		strings.Replace(string(raw), `"request_key":"`+testRequestKey+`"`, `"request_key":""`, 1),
		strings.Replace(string(raw), `"algorithm":"ES256"`, `"algorithm":"ES256","extra":1`, 1),
	}
	for index, candidate := range malformed {
		if _, err := DecodeStartBody([]byte(candidate)); err == nil {
			t.Fatalf("malformed start body %d accepted", index)
		}
	}
}

func TestDecodeStartReply(t *testing.T) {
	okRaw := `{"status":"ok","state":"started","intent_id":"` + testIntentID + `",` +
		`"credential_id":"cred-1","started_at":"2026-09-23T02:00:00+00:00",` +
		`"not_after":"2026-09-23T03:00:00Z","replay":true}`
	reply, apiError, err := DecodeStartReply([]byte(okRaw))
	if err != nil || apiError != nil {
		t.Fatalf("ok reply: %v / %v", err, apiError)
	}
	if !reply.Replay || !reply.NotAfter.Equal(reply.StartedAt.Add(time.Hour)) {
		t.Fatalf("ok reply wrong: %+v", reply)
	}
	reply, apiError, err = DecodeStartReply([]byte(`{"status":"error","code":"ONBOARDING_START_CONFLICT","retryable":true,"retry_after":10}`))
	if err != nil || apiError == nil || !errors.Is(apiError, ErrStartConflict) || !apiError.Retryable || apiError.RetryAfter != 10 {
		t.Fatalf("error reply wrong: %+v / %v", apiError, err)
	}
	if _, _, err := DecodeStartReply([]byte(`{"status":"ok","state":"pending","intent_id":"x","credential_id":"y","started_at":"2026-09-23T02:00:00Z","not_after":"2026-09-23T03:00:00Z","replay":false}`)); err == nil {
		t.Fatalf("non-started ok reply accepted")
	}
}

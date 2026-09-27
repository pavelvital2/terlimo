package onboarding

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"wg-turn-client/accountaccess"
	"wg-turn-client/wlwire"
)

// The golden fixtures below are the ACTUAL shapes of the accepted backend
// implementation committed with the extraction (backend 7548df78 / node 4397f3b):
// onboarding_api.create_intent appends request_id/server_time/schema_version to every
// poll, onboarding_hour.public_intent uses datetime.isoformat() (+00:00) times,
// evidence_transport replies {"status":"ok", ..., "message_hash"} and
// ONBOARDING_START_CONFLICT is the 409 replay-conflict code.

const (
	goldenReady = `{"request_id":"1f2e3d4c5b6a79880716253443526170","server_time":"2026-09-23T02:00:00.123456+00:00",` +
		`"schema_version":"0001_core","status":"ready","state":"ready","intent_id":"` + testIntentID + `",` +
		`"request_key":"` + testRequestKey + `","expires_at":"2026-09-23T02:10:00.123456+00:00",` +
		`"gateway":{"node_id":"terlimo-035-node","endpoint":{"peer_ip":"203.0.113.7","dtls_port":56002,` +
		`"dtls_spki_sha256":"` + testSPKI + `"}},` +
		`"credential_id":"0d1c2b3a-4f5e-6978-8796-a5b4c3d2e1f0",` +
		`"bootstrap":{"credential_id":"0d1c2b3a-4f5e-6978-8796-a5b4c3d2e1f0","secret":"ready-secret"},` +
		`"start_challenge":{"challenge_id":"00112233445566778899aabbccddeeff","nonce_b64":"` + goldenNonce + `",` +
		`"expires_at":"2026-09-23T02:09:00.123456+00:00"}}`

	goldenNonce = "c3VwZXItc2VjcmV0LW5vbmNlLXZhbHVlLTAwMDA"

	goldenStarted = `{"request_id":"1f2e3d4c5b6a79880716253443526170","server_time":"2026-09-23T02:01:00+00:00",` +
		`"schema_version":"0001_core","status":"started","state":"started","intent_id":"` + testIntentID + `",` +
		`"request_key":"` + testRequestKey + `","expires_at":"2026-09-23T02:10:00.123456+00:00",` +
		`"gateway":{"node_id":"terlimo-035-node","endpoint":{"peer_ip":"203.0.113.7","dtls_port":56002,` +
		`"dtls_spki_sha256":"` + testSPKI + `"}},"started_at":"2026-09-23T02:00:30.123456+00:00",` +
		`"not_after":"2026-09-23T03:00:30.123456+00:00"}`

	goldenFailed = `{"request_id":"1f2e3d4c5b6a79880716253443526170","server_time":"2026-09-23T02:01:00+00:00",` +
		`"schema_version":"0001_core","status":"failed","state":"failed","intent_id":"` + testIntentID + `",` +
		`"request_key":"` + testRequestKey + `","expires_at":"2026-09-23T02:10:00.123456+00:00",` +
		`"gateway":{"node_id":"terlimo-035-node","endpoint":{"peer_ip":"203.0.113.7","dtls_port":56002,` +
		`"dtls_spki_sha256":"` + testSPKI + `"}},"reason":"provision_dead"}`

	// evidence_transport._error_response(409 ONBOARDING_START_CONFLICT).
	goldenStartConflict = `{"status":"error","code":"ONBOARDING_START_CONFLICT","retryable":false}`

	// auth_api._error_response with request_id/server_time/schema_version + retry_after_ms.
	goldenIntentConflict = `{"request_id":"1f2e3d4c5b6a79880716253443526170","server_time":"2026-09-23T02:01:00Z",` +
		`"schema_version":"1.0","status":"error","code":"ONBOARDING_UNIT_ACTIVE","retryable":false}`

	goldenRateLimited = `{"request_id":"1f2e3d4c5b6a79880716253443526170","server_time":"2026-09-23T02:01:00Z",` +
		`"schema_version":"1.0","status":"error","code":"RATE_LIMITED","retryable":true,"retry_after_ms":60000}`
)

// evidence_transport ok reply (admit_evidence result spread under status:ok).
var goldenStartOK = `{"status":"ok","state":"started","replay":false,"intent_id":"` + testIntentID + `",` +
	`"credential_id":"0d1c2b3a-4f5e-6978-8796-a5b4c3d2e1f0",` +
	`"started_at":"2026-09-23T02:00:30.123456+00:00","not_after":"2026-09-23T03:00:30.123456+00:00",` +
	`"message_hash":"` + strings.Repeat("ab", 32) + `"}`

var goldenStartReplay = `{"status":"ok","state":"started","replay":true,"intent_id":"` + testIntentID + `",` +
	`"credential_id":"0d1c2b3a-4f5e-6978-8796-a5b4c3d2e1f0",` +
	`"started_at":"2026-09-23T02:00:30.123456+00:00","not_after":"2026-09-23T03:00:30.123456+00:00",` +
	`"connection_id_hash":"` + strings.Repeat("cd", 32) + `","message_hash":"` + strings.Repeat("ab", 32) + `"}`

func TestGoldenBackendPollShapesDecode(t *testing.T) {
	ready, err := DecodePoll([]byte(goldenReady))
	if err != nil || ready.State != StateReady || ready.IntentID != testIntentID ||
		ready.RequestKey != testRequestKey || ready.Bootstrap == nil || ready.Bootstrap.Secret != "ready-secret" ||
		ready.CredentialID != "0d1c2b3a-4f5e-6978-8796-a5b4c3d2e1f0" || ready.StartChallenge == nil ||
		ready.StartChallenge.ChallengeID != "00112233445566778899aabbccddeeff" ||
		ready.Gateway == nil || ready.Gateway.NodeID != "terlimo-035-node" || ready.Gateway.Endpoint.DTLSPort != 56002 {
		t.Fatalf("golden ready decode: %+v err=%v", ready, err)
	}
	started, err := DecodePoll([]byte(goldenStarted))
	if err != nil || started.State != StateStarted || started.StartedAt.IsZero() || started.NotAfter.IsZero() ||
		!started.NotAfter.After(started.StartedAt) {
		t.Fatalf("golden started decode: %+v err=%v", started, err)
	}
	failed, err := DecodePoll([]byte(goldenFailed))
	if err != nil || failed.State != StateFailed || failed.FailureReason != "provision_dead" {
		t.Fatalf("golden failed decode: %+v err=%v", failed, err)
	}
}

func TestGoldenBackendErrorShapesDecode(t *testing.T) {
	conflict, err := DecodeError([]byte(goldenIntentConflict), 409)
	if err != nil || conflict.Code != "ONBOARDING_UNIT_ACTIVE" || conflict.HTTPStatus != 409 || conflict.Retryable {
		t.Fatalf("intent conflict: %+v err=%v", conflict, err)
	}
	limited, err := DecodeError([]byte(goldenRateLimited), 429)
	if err != nil || limited.Code != "RATE_LIMITED" || limited.RetryAfter != 60 {
		t.Fatalf("rate limited: %+v err=%v", limited, err)
	}
	startConflict, err := DecodeError([]byte(goldenStartConflict), 0)
	if err != nil || !errors.Is(startConflict, ErrStartConflict) || startConflict.RetryAfter != 0 {
		t.Fatalf("start conflict: %+v err=%v", startConflict, err)
	}
}

func TestGoldenBackendStartReplyDecodes(t *testing.T) {
	reply, apiError, err := DecodeStartReply([]byte(goldenStartOK))
	if err != nil || apiError != nil || reply.Replay || reply.MessageHash != strings.Repeat("ab", 32) ||
		reply.IntentID != testIntentID || reply.CredentialID != "0d1c2b3a-4f5e-6978-8796-a5b4c3d2e1f0" {
		t.Fatalf("start ok: %+v %+v %v", reply, apiError, err)
	}
	replay, apiError, err := DecodeStartReply([]byte(goldenStartReplay))
	if err != nil || apiError != nil || !replay.Replay || replay.ConnectionIDHash != strings.Repeat("cd", 32) {
		t.Fatalf("start replay: %+v %+v %v", replay, apiError, err)
	}
	_, apiError, err = DecodeStartReply([]byte(goldenStartConflict))
	if err != nil || apiError == nil || !errors.Is(apiError, ErrStartConflict) {
		t.Fatalf("start conflict reply: %+v %v", apiError, err)
	}
}

// nodeCompatibleListener mimics the extracted node handler client_test_transport.go
// (clientTestBootstrapServe): it reassembles one WLBS request frame, forms the backend
// envelope with the NODE's own connection_id and request_id, hands the opaque body to
// the "backend" check, and returns the backend bytes in a response frame with the same
// tunnel id. The client never sends credential_id/connection_id/request_id itself.
type nodeCompatibleListener struct {
	ln       net.Listener
	received chan map[string]any
	bodies   chan []byte
	reply    func(body []byte, attempt int) ([]byte, bool)
	session  string
}

func newNodeCompatibleListener(t *testing.T, reply func(body []byte, attempt int) ([]byte, bool)) *nodeCompatibleListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	session := make([]byte, 16)
	if _, err := rand.Read(session); err != nil {
		t.Fatalf("session: %v", err)
	}
	l := &nodeCompatibleListener{ln: ln, received: make(chan map[string]any, 8), bodies: make(chan []byte, 8),
		reply: reply, session: hex.EncodeToString(session)}
	t.Cleanup(func() { _ = ln.Close() })
	go l.serve(t)
	return l
}

func (l *nodeCompatibleListener) addr() string { return l.ln.Addr().String() }

func (l *nodeCompatibleListener) dialer() func(ctx context.Context, endpoint GatewayEndpoint, bootstrap Bootstrap) (net.Conn, func(), error) {
	return func(ctx context.Context, endpoint GatewayEndpoint, bootstrap Bootstrap) (net.Conn, func(), error) {
		conn, err := net.Dial("tcp", l.addr())
		if err != nil {
			return nil, nil, err
		}
		return conn, func() { _ = conn.Close() }, nil
	}
}

func (l *nodeCompatibleListener) serve(t *testing.T) {
	t.Helper()
	attempt := 0
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return
		}
		attempt++
		go func(conn net.Conn, attempt int) {
			defer conn.Close()
			var assembler wlwire.Assembler
			buffer := make([]byte, 32+wlwire.Fragment+1)
			var frameID wlwire.ID
			var body []byte
			var pending []byte
			for body == nil {
				n, err := conn.Read(buffer)
				if err != nil {
					return
				}
				pending = append(pending, buffer[:n]...)
				for body == nil {
					frame, rest, complete := nextWireFrame(pending)
					if !complete {
						break
					}
					pending = rest
					frameID, body, err = assembler.Add(frame, time.Now())
					if err != nil {
						return
					}
				}
			}
			envelope := map[string]any{
				"credential_id": "0d1c2b3a-4f5e-6978-8796-a5b4c3d2e1f0",
				"connection_id": l.session,
				"request_id":    wlwire.Encode(frameID[:]),
				"body":          json.RawMessage(body),
			}
			l.received <- envelope
			l.bodies <- append([]byte(nil), body...)
			reply, send := l.reply(body, attempt)
			if !send {
				return
			}
			frames, err := wlwire.Frames(frameID, true, reply)
			if err != nil {
				return
			}
			for _, frame := range frames {
				if _, err := conn.Write(frame); err != nil {
					return
				}
			}
		}(conn, attempt)
	}
}

func testIdentity(t *testing.T) (Identity, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("spki: %v", err)
	}
	identity := Identity{
		Environment:    EnvironmentTest,
		InstallationID: accountaccess.InstallationFingerprint(spki),
		Signer: func(_ context.Context, transcript []byte) ([]byte, error) {
			digest := sha256.Sum256(transcript)
			return ecdsa.SignASN1(rand.Reader, key, digest[:])
		},
		Now: func() time.Time { return time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC) },
	}
	return identity, key, spki
}

func goldenReadyPoll(t *testing.T) IntentPoll {
	t.Helper()
	poll, err := DecodePoll([]byte(goldenReady))
	if err != nil {
		t.Fatalf("ready golden: %v", err)
	}
	return poll
}

func TestBootstrapStarterRealPathOverNodeCompatibleListener(t *testing.T) {
	identity, _, spki := testIdentity(t)
	listener := newNodeCompatibleListener(t, func(body []byte, _ int) ([]byte, bool) {
		return []byte(goldenStartOK), true
	})
	starter := &BootstrapStarter{Identity: identity, Dial: listener.dialer()}
	reply, apiError, err := starter.Start(context.Background(), goldenReadyPoll(t))
	if err != nil || apiError != nil {
		t.Fatalf("start: %v %v", apiError, err)
	}
	if reply.IntentID != testIntentID || reply.Replay || reply.MessageHash != strings.Repeat("ab", 32) {
		t.Fatalf("reply: %+v", reply)
	}

	envelope := <-listener.received
	bodyRaw := <-listener.bodies
	// The node forms the envelope; the client body must not contain its fields.
	if envelope["connection_id"] != listener.session || len(listener.session) != 32 {
		t.Fatalf("node connection_id missing: %+v", envelope)
	}
	if _, ok := envelope["body"].(json.RawMessage); !ok {
		t.Fatalf("body not verbatim: %T", envelope["body"])
	}
	if strings.Contains(string(bodyRaw), "connection_id") || strings.Contains(string(bodyRaw), "credential_id") {
		t.Fatalf("client invented envelope fields: %s", bodyRaw)
	}

	// Golden start body (rev3 section 2 / extracted _start_envelope).
	decoded, err := DecodeStartBody(bodyRaw)
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	expected := map[string]any{
		"env":             EnvironmentTest,
		"scope":           IntentScope,
		"op":              StartOp,
		"installation_id": identity.InstallationID,
		"intent_id":       testIntentID,
		"request_key":     testRequestKey,
		"ts":              "2026-09-23T02:00:00Z",
		"nonce":           goldenNonce,
		"request_id":      decoded.Proof.RequestID,
	}
	canonical, err := accountaccess.CanonicalJSON(expected)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if decoded.Proof.SignedPayloadB64 != accountaccess.B64URLEncode(canonical) {
		t.Fatalf("signed payload mismatch: %s", decoded.Proof.SignedPayloadB64)
	}
	message, err := accountaccess.POPMessage(decoded.Proof.RequestID, decoded.Proof.ChallengeID, decoded.Proof.NonceB64, canonical)
	if err != nil {
		t.Fatalf("pop message: %v", err)
	}
	signature, err := accountaccess.B64URLDecodeStrict(decoded.Proof.SignatureB64, -1)
	if err != nil {
		t.Fatalf("signature decode: %v", err)
	}
	if err := wlwire.Verify(spki, message, signature); err != nil {
		t.Fatalf("proof verify: %v", err)
	}
	sum := sha256.Sum256(canonical)
	if decoded.Proof.PayloadHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("payload hash: %s", decoded.Proof.PayloadHash)
	}
}

func TestBootstrapStarterRetriesExactSameSignedRequest(t *testing.T) {
	identity, _, _ := testIdentity(t)
	listener := newNodeCompatibleListener(t, func(_ []byte, attempt int) ([]byte, bool) {
		if attempt == 1 {
			return nil, false // lost response: first tunnel closes without a reply
		}
		return []byte(goldenStartReplay), true
	})
	starter := &BootstrapStarter{Identity: identity, Dial: listener.dialer(), AttemptTimeout: 2 * time.Second}
	reply, apiError, err := starter.Start(context.Background(), goldenReadyPoll(t))
	if err != nil || apiError != nil || !reply.Replay {
		t.Fatalf("retry start: %+v %+v %v", reply, apiError, err)
	}
	first := <-listener.bodies
	second := <-listener.bodies
	if string(first) != string(second) {
		t.Fatalf("retry changed the signed request:\n%s\n%s", first, second)
	}
}

func TestBootstrapStarterKeepsExactConflictCode(t *testing.T) {
	identity, _, _ := testIdentity(t)
	listener := newNodeCompatibleListener(t, func(_ []byte, _ int) ([]byte, bool) {
		return []byte(goldenStartConflict), true
	})
	starter := &BootstrapStarter{Identity: identity, Dial: listener.dialer()}
	_, apiError, err := starter.Start(context.Background(), goldenReadyPoll(t))
	if err != nil || apiError == nil || !errors.Is(apiError, ErrStartConflict) || apiError.HTTPStatus != 0 {
		t.Fatalf("conflict: %+v %v", apiError, err)
	}
}

func TestBootstrapStarterFailsClosedWithoutTransport(t *testing.T) {
	identity, _, _ := testIdentity(t)
	starter := &BootstrapStarter{Identity: identity}
	if _, _, err := starter.Start(context.Background(), goldenReadyPoll(t)); !errors.Is(err, ErrStartUnavailable) {
		t.Fatalf("stub start: %v", err)
	}
	noTransport := func(context.Context, GatewayEndpoint, Bootstrap) (net.Conn, func(), error) {
		return nil, nil, errors.New("no transport")
	}
	startable := &BootstrapStarter{Identity: identity, Dial: noTransport}
	if _, _, err := startable.Start(context.Background(), IntentPoll{State: StatePending}); !errors.Is(err, ErrStartNotReady) {
		t.Fatalf("not ready: %v", err)
	}
}

// fixedIntentClient is the accepted IntentClient adapter contract with a golden ready
// poll: it binds the caller's durable request_key to the poll identity and returns the
// same intent state on retry, exactly as the backend public_intent route promises.
type fixedIntentClient struct{ poll IntentPoll }

func (f fixedIntentClient) Intent(_ context.Context, requestKey, _ string) (IntentPoll, *APIError, error) {
	poll := f.poll
	poll.RequestKey = requestKey
	return poll, nil, nil
}

// TestFlowConnectStartThroughRealTransport proves the whole explicit path: the Flow
// state machine creates/reuses the durable request_key, receives a golden ready poll
// through the adapter seam, and performs the signed start over the real
// BootstrapStarter wire exchange (node-formed envelope, backend golden reply).
func TestFlowConnectStartThroughRealTransport(t *testing.T) {
	identity, _, _ := testIdentity(t)
	listener := newNodeCompatibleListener(t, func(_ []byte, _ int) ([]byte, bool) {
		return []byte(goldenStartOK), true
	})
	starter := &BootstrapStarter{Identity: identity, Dial: listener.dialer()}
	flow, err := NewFlow(&MemoryStore{}, fixedIntentClient{poll: goldenReadyPoll(t)}, starter, Options{})
	if err != nil {
		t.Fatalf("flow: %v", err)
	}
	result, err := flow.Connect(context.Background())
	if err != nil || result.State != StateReady || result.Poll == nil {
		t.Fatalf("connect: %+v %v", result, err)
	}
	reply, err := flow.Start(context.Background(), *result.Poll)
	if err != nil || reply.Replay || reply.IntentID != testIntentID {
		t.Fatalf("start: %+v %v", reply, err)
	}
	if state := flow.State(); state != StateStarted {
		t.Fatalf("state: %s", state)
	}
	envelope := <-listener.received
	if envelope["connection_id"] != listener.session {
		t.Fatalf("node envelope: %+v", envelope)
	}
}

// TestFlowStartNeverFallsBackWithoutTransport keeps the explicit start fail-closed
// when no trusted bootstrap transport is configured.
func TestFlowStartNeverFallsBackWithoutTransport(t *testing.T) {
	flow, err := NewFlow(&MemoryStore{}, fixedIntentClient{poll: goldenReadyPoll(t)}, nil, Options{})
	if err != nil {
		t.Fatalf("flow: %v", err)
	}
	result, err := flow.Connect(context.Background())
	if err != nil || result.State != StateReady {
		t.Fatalf("connect: %+v %v", result, err)
	}
	if _, err := flow.Start(context.Background(), *result.Poll); !errors.Is(err, ErrStartUnavailable) {
		t.Fatalf("start without transport: %v", err)
	}
}

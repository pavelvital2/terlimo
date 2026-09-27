package onboarding

// Real-HTTP wire verification of the CORRECTION1 gateway_key placement. A local
// httptest.Server stands in for the accepted backend route and the ACTUAL signed intent
// payload bytes the client sends are captured, both for the initial POST and the
// follow-up POLL. This is deliberately not a map-builder test: the assertions run on the
// canonical bytes and decoded signed payload that went over the wire, and the expected
// key sets come from the hermetic accepted-server fixture
// go_client/testdata/step036/step036_intent_payload_gateway_key.json (sha256 pinned in
// step036_fixture_test.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"wg-turn-client/accountaccess"
)

type capturedIntentPayload struct {
	raw    []byte
	fields map[string]any
}

type wireIntentServer struct {
	mu       sync.Mutex
	answered int
	captured []capturedIntentPayload
	selected string
}

func newWireIntentServer(t *testing.T, selected string) (*httptest.Server, *wireIntentServer) {
	t.Helper()
	wire := &wireIntentServer{selected: selected}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case ChallengePath:
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(challengeGolden))
		case IntentPath:
			var envelope struct {
				Proof map[string]any `json:"proof"`
			}
			if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
				http.Error(writer, "bad envelope", http.StatusBadRequest)
				return
			}
			signed, ok := envelope.Proof["signed_payload_b64"].(string)
			if !ok {
				http.Error(writer, "missing proof", http.StatusBadRequest)
				return
			}
			raw, fields, err := accountaccess.DecodeProofPayload(signed)
			if err != nil {
				http.Error(writer, "non-canonical payload", http.StatusBadRequest)
				return
			}
			wire.mu.Lock()
			wire.captured = append(wire.captured, capturedIntentPayload{raw: raw, fields: fields})
			wire.answered++
			answer := wire.answered
			selected := wire.selected
			wire.mu.Unlock()
			requestKey, _ := fields["request_key"].(string)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write(wire.pollResponse(requestKey, selected, answer > 1))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	return server, wire
}

func (w *wireIntentServer) pollResponse(requestKey, selected string, ready bool) []byte {
	gateway := map[string]any{"node_id": selected, "endpoint": map[string]any{
		"peer_ip": "203.0.113.7", "dtls_port": 56002, "dtls_spki_sha256": testSPKI}}
	envelope := map[string]any{
		"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2026-09-23T02:00:00Z",
		"schema_version": "1", "intent_id": testIntentID, "request_key": requestKey,
		"expires_at": "2026-09-23T03:00:00Z", "gateway": gateway,
	}
	if !ready {
		envelope["status"] = "pending"
		envelope["state"] = "pending"
		envelope["retry_after"] = 0
	} else {
		envelope["status"] = "ready"
		envelope["state"] = "ready"
		envelope["credential_id"] = "cred-1"
		envelope["bootstrap"] = map[string]any{"credential_id": "cred-1", "secret": "c2VjcmV0"}
		envelope["start_challenge"] = map[string]any{
			"challenge_id": "00112233445566778899aabbccddeeff", "nonce_b64": "bm9uY2U",
			"expires_at": "2026-09-23T02:10:00Z"}
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		panic(err)
	}
	return raw
}

func (w *wireIntentServer) payloads() []capturedIntentPayload {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]capturedIntentPayload(nil), w.captured...)
}

func wireGatewayClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := NewClient(http.DefaultClient, accountaccess.StaticToken("session-bearer"), baseURL,
		Identity{Environment: EnvironmentTest, InstallationID: strings.Repeat("ab", 32),
			Signer: func(context.Context, []byte) ([]byte, error) { return make([]byte, 64), nil },
			Now:    func() time.Time { return time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return client
}

// TestWireIntentCarriesGatewayKeyOnEveryPostAndPoll drives the real client through a
// pending -> ready intent flow for the fixture selection (gw-beta, not the first gateway
// of the fixture catalog) and captures both signed payloads: the initial POST and the
// follow-up POLL.
func TestWireIntentCarriesGatewayKeyOnEveryPostAndPoll(t *testing.T) {
	fixture := step036IntentFixture(t)
	wantKeys := sortedFieldNames(fixture["with_gateway_key"])
	selection, ok := fixture["with_gateway_key"]["gateway_key"].(string)
	if !ok || selection == "" {
		t.Fatalf("fixture with_gateway_key must select a gateway: %v", fixture["with_gateway_key"])
	}
	t.Logf("server fixture with_gateway_key selects %q with keys %v", selection, wantKeys)
	server, wire := newWireIntentServer(t, selection)
	flow := bindingFlow(t, &MemoryStore{}, wireGatewayClient(t, server.URL), nil)

	result, err := flow.ConnectWithGateway(context.Background(), selection)
	if err != nil || result.State != StateReady || result.Poll == nil {
		t.Fatalf("wire connect: %+v / %v", result, err)
	}
	captured := wire.payloads()
	if len(captured) != 2 {
		t.Fatalf("want one initial POST and one POLL captured, got %d", len(captured))
	}
	for index, payload := range captured {
		if payload.fields["gateway_key"] != selection {
			t.Fatalf("payload %d gateway_key = %v, want %q", index, payload.fields["gateway_key"], selection)
		}
		if payload.fields["request_key"] != result.Poll.RequestKey {
			t.Fatalf("payload %d request_key = %v, want %q", index, payload.fields["request_key"], result.Poll.RequestKey)
		}
		if payload.fields["scope"] != IntentScope || payload.fields["op"] != IntentOp {
			t.Fatalf("payload %d scope/op changed: %v", index, payload.fields)
		}
		// The actual signed bytes carry EXACTLY the fixture with_gateway_key key set.
		if got := sortedFieldNames(payload.fields); !reflect.DeepEqual(got, wantKeys) {
			t.Fatalf("payload %d key set = %v, fixture with_gateway_key = %v", index, got, wantKeys)
		}
	}

	// Retrying the same request_key keeps the same durable pairing.
	second, err := flow.ConnectWithGateway(context.Background(), selection)
	if err != nil || second.State != StateReady {
		t.Fatalf("wire retry: %+v / %v", second, err)
	}
	retried := wire.payloads()
	if len(retried) != 3 {
		t.Fatalf("want the retry captured too, got %d payloads", len(retried))
	}
	last := retried[2]
	if last.fields["gateway_key"] != selection || last.fields["request_key"] != result.Poll.RequestKey {
		t.Fatalf("retry changed the durable pairing: %v", last.fields)
	}
}

// TestWireLegacyIntentSendsNoGatewayKeyField proves byte-level absence: a caller without
// a selection posts the unchanged legacy payload, its key set equals the fixture
// legacy_without_gateway_key set, and the captured canonical bytes never contain the
// field name.
func TestWireLegacyIntentSendsNoGatewayKeyField(t *testing.T) {
	fixture := step036IntentFixture(t)
	wantKeys := sortedFieldNames(fixture["legacy_without_gateway_key"])
	server, wire := newWireIntentServer(t, "gw-alpha")
	flow := bindingFlow(t, &MemoryStore{}, wireGatewayClient(t, server.URL), nil)

	result, err := flow.Connect(context.Background())
	if err != nil || result.State != StateReady {
		t.Fatalf("legacy wire connect: %+v / %v", result, err)
	}
	captured := wire.payloads()
	if len(captured) != 2 {
		t.Fatalf("want two legacy payloads, got %d", len(captured))
	}
	for index, payload := range captured {
		if strings.Contains(string(payload.raw), "gateway_key") {
			t.Fatalf("legacy payload %d leaked gateway_key: %s", index, payload.raw)
		}
		if _, present := payload.fields["gateway_key"]; present {
			t.Fatalf("legacy payload %d carries gateway_key: %v", index, payload.fields)
		}
		if got := sortedFieldNames(payload.fields); !reflect.DeepEqual(got, wantKeys) {
			t.Fatalf("legacy payload %d key set = %v, fixture legacy_without_gateway_key = %v", index, got, wantKeys)
		}
	}
}

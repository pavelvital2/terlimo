package onboarding

// Focused tests for the owner-contract gateway binding after CORRECTION1: the optional
// gateway_key of the signed INTENT request is sent with the request_key on every POST
// and POLL, the durable request_key+gateway_key pairing is immutable, the start payload
// keeps the accepted legacy bytes (no gateway_key), the returned intent gateway is
// checked against the durable pairing before any bootstrap/start, and the accepted
// expiry recovery never resurrects the old pairing.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"wg-turn-client/accountaccess"
)

type bindingStarter struct {
	mu     sync.Mutex
	calls  int
	last   IntentPoll
	reply  StartReply
	apiErr *APIError
	err    error
}

func (s *bindingStarter) Start(_ context.Context, ready IntentPoll) (StartReply, *APIError, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.last = ready
	return s.reply, s.apiErr, s.err
}

func (s *bindingStarter) lastPoll() IntentPoll {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *bindingStarter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func bindingFlow(t *testing.T, store Store, intents IntentClient, starter Starter) *Flow {
	t.Helper()
	flow, err := NewFlow(store, intents, starter, Options{Sleep: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatalf("flow: %v", err)
	}
	return flow
}

// gatewayBoundPoll clones one poll with the returned gateway node_id set to gatewayKey.
// On the wire gateway.node_id is the stable public gateway key the intent is bound to.
func gatewayBoundPoll(poll IntentPoll, gatewayKey string) IntentPoll {
	if poll.Gateway != nil {
		gateway := *poll.Gateway
		gateway.NodeID = gatewayKey
		poll.Gateway = &gateway
	}
	return poll
}

func startReplyFixture() StartReply {
	return StartReply{IntentID: testIntentID, CredentialID: "cred-1",
		StartedAt: time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC),
		NotAfter:  time.Date(2026, 9, 23, 3, 0, 0, 0, time.UTC)}
}

func TestGatewayKeyPayloadStaysOptionalAndExact(t *testing.T) {
	legacy := IntentPayload(EnvironmentTest, "inst", "key", "nonce", "req", "ts", "")
	if _, ok := legacy["gateway_key"]; ok {
		t.Fatal("callers without a selection must not send gateway_key")
	}
	if len(legacy) != 8 || legacy["scope"] != IntentScope || legacy["op"] != IntentOp {
		t.Fatalf("legacy intent payload changed: %v", legacy)
	}
	bound := IntentPayload(EnvironmentTest, "inst", "key", "nonce", "req", "ts", "gw-a")
	if bound["gateway_key"] != "gw-a" || len(bound) != 9 {
		t.Fatalf("gateway_key not bound: %v", bound)
	}
	start := StartPayload(EnvironmentTest, "inst", "key", testIntentID, "nonce", "req", "ts")
	if _, ok := start["gateway_key"]; ok {
		t.Fatalf("the signed start payload must keep the accepted legacy bytes: %v", start)
	}
	if len(start) != 9 || start["op"] != StartOp || start["intent_id"] != testIntentID ||
		start["request_key"] != "key" || start["scope"] != IntentScope {
		t.Fatalf("start payload field set changed: %v", start)
	}
}

// TestSignStartBodyKeepsAcceptedLegacyBytes pins the exact canonical start payload: the
// field set and order are frozen by the accepted server verification, so this literal is
// the regression guard against re-adding gateway_key (or any field) to the start RPC.
func TestSignStartBodyKeepsAcceptedLegacyBytes(t *testing.T) {
	identity := Identity{Environment: EnvironmentTest, InstallationID: "inst",
		Signer: func(context.Context, []byte) ([]byte, error) { return make([]byte, 64), nil },
		Now:    func() time.Time { return time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC) }}
	challenge := StartChallenge{ChallengeID: "ffeeddccbbaa99887766554433221100", NonceB64: "bm9uY2U"}
	const requestID = "00112233445566778899aabbccddeeff"
	body, err := SignStartBody(context.Background(), identity, testIntentID, testRequestKey, challenge,
		challenge.NonceB64, requestID)
	if err != nil {
		t.Fatal(err)
	}
	raw, payload, err := accountaccess.DecodeProofPayload(body.Proof.SignedPayloadB64)
	if err != nil {
		t.Fatalf("legacy payload not canonical: %v", err)
	}
	const golden = `{"env":"test","installation_id":"inst","intent_id":"` + testIntentID +
		`","nonce":"bm9uY2U","op":"onboarding.start","request_id":"` + requestID +
		`","request_key":"` + testRequestKey + `","scope":"onboarding:start","ts":"2026-09-23T02:00:00Z"}`
	if string(raw) != golden {
		t.Fatalf("start signed payload changed:\n got %s\nwant %s", raw, golden)
	}
	if _, ok := payload["gateway_key"]; ok {
		t.Fatalf("start payload must never carry gateway_key: %v", payload)
	}
	if body.Proof.RequestID != requestID || body.Proof.ChallengeID != challenge.ChallengeID ||
		body.Proof.NonceB64 != "bm9uY2U" || body.Proof.PayloadHash == "" {
		t.Fatalf("start proof binding wrong: %+v", body.Proof)
	}
}

func TestFlowGatewayPairingReuseAndLocalConflict(t *testing.T) {
	store := &MemoryStore{}
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return gatewayBoundPoll(readyPoll(key), "gw-a"), nil, nil
	}}
	starter := &bindingStarter{reply: startReplyFixture()}
	flow := bindingFlow(t, store, intents, starter)
	ctx := context.Background()

	first, err := flow.ConnectWithGateway(ctx, "gw-a")
	if err != nil || first.State != StateReady {
		t.Fatalf("first connect: %+v / %v", first, err)
	}
	state, _ := store.Load()
	if state.RequestKey == "" || state.GatewayKey != "gw-a" {
		t.Fatalf("pairing not persisted: %+v", state)
	}
	requestKey := state.RequestKey
	if got := intents.sentGateways(); len(got) != 1 || got[0] != "gw-a" {
		t.Fatalf("first intent POST must carry the durable gateway: %v", got)
	}

	second, err := flow.ConnectWithGateway(ctx, "gw-a")
	if err != nil || second.State != StateReady {
		t.Fatalf("same-key retry must reuse the intent: %+v / %v", second, err)
	}
	if intents.count() != 2 || intents.calls[0] != requestKey || intents.calls[1] != requestKey {
		t.Fatalf("request_key not reused: %v", intents.calls)
	}
	if got := intents.sentGateways(); len(got) != 2 || got[1] != "gw-a" {
		t.Fatalf("retry poll must carry the durable gateway: %v", got)
	}

	// A different gateway for the unfinished intent is a local conflict with no
	// network call and no state change.
	_, err = flow.ConnectWithGateway(ctx, "gw-b")
	if !errors.Is(err, ErrIntentConflict) {
		t.Fatalf("different gateway must pre-empt locally: %v", err)
	}
	if intents.count() != 2 {
		t.Fatalf("conflict must not reach the network: %v", intents.calls)
	}
	kept, _ := store.Load()
	if kept.RequestKey != requestKey || kept.GatewayKey != "gw-a" {
		t.Fatalf("conflict must keep the old pairing: %+v", kept)
	}

	// An empty incoming key from a legacy caller reuses the durable pairing.
	if _, err := flow.Connect(ctx); err != nil {
		t.Fatalf("legacy empty caller must reuse the stored pairing: %v", err)
	}
	if intents.count() != 3 || intents.calls[2] != requestKey {
		t.Fatalf("legacy caller changed the request_key: %v", intents.calls)
	}
	if got := intents.sentGateways(); got[2] != "gw-a" {
		t.Fatalf("legacy caller must reuse the stored gateway pairing: %v", got)
	}

	// Start consumes the same durable attempt; the returned gateway matched the pairing.
	if _, err := flow.Start(ctx, *second.Poll); err != nil {
		t.Fatalf("start: %v", err)
	}
	if starter.count() != 1 {
		t.Fatalf("start must reach the starter exactly once: %d", starter.count())
	}
	started, _ := store.Load()
	if started.StartedAt == "" || started.GatewayKey != "gw-a" {
		t.Fatalf("started attempt must keep the durable pairing: %+v", started)
	}
}

func TestFlowUnfinishedIntentWithoutPairingConflictsOnFirstSelection(t *testing.T) {
	store := &MemoryStore{}
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return readyPoll(key), nil, nil
	}}
	flow := bindingFlow(t, store, intents, &bindingStarter{reply: startReplyFixture()})
	ctx := context.Background()

	if _, err := flow.Connect(ctx); err != nil {
		t.Fatalf("keyless legacy connect: %v", err)
	}
	state, _ := store.Load()
	if state.GatewayKey != "" {
		t.Fatalf("keyless attempt must not invent a pairing: %+v", state)
	}
	// Selecting a gateway after the keyless attempt began is a change of the
	// unfinished attempt: reported as a conflict, never a silent switch.
	if _, err := flow.ConnectWithGateway(ctx, "gw-b"); !errors.Is(err, ErrIntentConflict) {
		t.Fatalf("selection change on an unfinished attempt must conflict: %v", err)
	}
	if intents.count() != 1 {
		t.Fatalf("conflict must not reach the network: %v", intents.calls)
	}
}

// TestFlowIntentPollsCarryDurableGatewayKey proves the CORRECTION1 wire placement: both
// the initial intent POST and the follow-up POLL carry the selection of a non-first
// gateway with the same request_key.
func TestFlowIntentPollsCarryDurableGatewayKey(t *testing.T) {
	store := &MemoryStore{}
	intents := &scriptedIntent{step: func(n int, key string) (IntentPoll, *APIError, error) {
		if n == 1 {
			return gatewayBoundPoll(pendingPoll(key), "gw-003"), nil, nil
		}
		return gatewayBoundPoll(readyPoll(key), "gw-003"), nil, nil
	}}
	starter := &bindingStarter{reply: startReplyFixture()}
	flow := bindingFlow(t, store, intents, starter)

	result, err := flow.ConnectWithGateway(context.Background(), "gw-003")
	if err != nil || result.State != StateReady || result.Poll == nil {
		t.Fatalf("connect: %+v / %v", result, err)
	}
	if intents.count() != 2 {
		t.Fatalf("want one POST and one POLL, got %d calls", intents.count())
	}
	gateways := intents.sentGateways()
	if len(gateways) != 2 || gateways[0] != "gw-003" || gateways[1] != "gw-003" {
		t.Fatalf("every intent POST/POLL must carry the selected gateway: %v", gateways)
	}
	if intents.calls[0] != intents.calls[1] || intents.calls[0] != result.Poll.RequestKey {
		t.Fatalf("request_key must be stable across POST/POLL: %v vs %q", intents.calls, result.Poll.RequestKey)
	}
	if starter.count() != 0 {
		t.Fatalf("Connect must never call the start RPC")
	}
}

// TestFlowReturnedGatewayMismatchConflictsBeforeStart proves the returned-binding guard:
// when the server answers an intent for the durable pairing with another gateway, the
// flow reports ErrIntentConflict before any state transition or bootstrap/start and the
// stored pairing stays untouched.
func TestFlowReturnedGatewayMismatchConflictsBeforeStart(t *testing.T) {
	seed := KeyState{RequestKey: "rk-returned", IntentID: testIntentID, GatewayKey: "gw-a"}
	store := &MemoryStore{state: seed, set: true}
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return gatewayBoundPoll(readyPoll(key), "gw-b"), nil, nil
	}}
	starter := &bindingStarter{reply: startReplyFixture()}
	flow := bindingFlow(t, store, intents, starter)

	// The intent answer returns gw-b while the durable pairing is gw-a.
	if _, err := flow.Connect(context.Background()); !errors.Is(err, ErrIntentConflict) {
		t.Fatalf("returned gateway mismatch must conflict: %v", err)
	}
	if got := intents.sentGateways(); len(got) != 1 || got[0] != "gw-a" {
		t.Fatalf("the intent call must carry the durable pairing: %v", got)
	}
	if starter.count() != 0 {
		t.Fatalf("a mismatch must never reach the starter")
	}
	after, _ := store.Load()
	if after != seed {
		t.Fatalf("a mismatch must not mutate the durable attempt: %+v", after)
	}

	// Recover observes the same answer and must fail the same way.
	if _, err := flow.Recover(context.Background()); !errors.Is(err, ErrIntentConflict) {
		t.Fatalf("recover must enforce the same binding: %v", err)
	}
	if after, _ := store.Load(); after != seed {
		t.Fatalf("recover must not mutate the durable attempt: %+v", after)
	}

	// A ready poll handed straight to Start is checked before the bootstrap RPC.
	ready := gatewayBoundPoll(readyPoll("rk-returned"), "gw-b")
	if _, err := flow.Start(context.Background(), ready); !errors.Is(err, ErrIntentConflict) {
		t.Fatalf("start must reject a mismatching returned gateway: %v", err)
	}
	if starter.count() != 0 {
		t.Fatalf("start mismatch must never reach the starter")
	}
}

func TestFlowExpiredRecoveryCreatesNewKeyAndNeverResurrects(t *testing.T) {
	store := &MemoryStore{}
	intents := &scriptedIntent{step: func(n int, key string) (IntentPoll, *APIError, error) {
		if n == 1 {
			return IntentPoll{}, &APIError{HTTPStatus: 410, Code: "ONBOARDING_INTENT_EXPIRED"}, nil
		}
		return gatewayBoundPoll(readyPoll(key), "gw-b"), nil, nil
	}}
	starter := &bindingStarter{reply: startReplyFixture()}
	flow := bindingFlow(t, store, intents, starter)
	ctx := context.Background()

	_, err := flow.ConnectWithGateway(ctx, "gw-a")
	if !errors.Is(err, ErrIntentExpired) {
		t.Fatalf("expired intent must keep its code: %v", err)
	}
	expired, _ := store.Load()
	if expired.Terminal != string(StateExpired) || expired.GatewayKey != "gw-a" || expired.RequestKey == "" {
		t.Fatalf("expired pairing not persisted: %+v", expired)
	}

	second, err := flow.ConnectWithGateway(ctx, "gw-b")
	if err != nil || second.State != StateReady {
		t.Fatalf("expiry recovery: %+v / %v", second, err)
	}
	if intents.count() != 2 || intents.calls[1] == expired.RequestKey {
		t.Fatalf("expiry recovery must mint a new request_key: %v", intents.calls)
	}
	recovered, _ := store.Load()
	if recovered.RequestKey != intents.calls[1] || recovered.GatewayKey != "gw-b" || recovered.Terminal != "" {
		t.Fatalf("recovered pairing wrong: %+v", recovered)
	}
	if got := intents.sentGateways(); len(got) != 2 || got[0] != "gw-a" || got[1] != "gw-b" {
		t.Fatalf("recovery must send the new pairing, not resurrect the old: %v", got)
	}
	if _, err := flow.Start(ctx, *second.Poll); err != nil {
		t.Fatalf("start after recovery: %v", err)
	}
	if starter.lastPoll().RequestKey != recovered.RequestKey {
		t.Fatalf("start correlation wrong: %+v", starter.lastPoll())
	}
}

func TestIntentConflictSentinelMatchesServer409(t *testing.T) {
	if !errors.Is(&APIError{HTTPStatus: 409, Code: "ONBOARDING_INTENT_CONFLICT"}, ErrIntentConflict) {
		t.Fatal("server 409 ONBOARDING_INTENT_CONFLICT must match the sentinel")
	}
	if ErrIntentConflict.Error() != "ONBOARDING_INTENT_CONFLICT" {
		t.Fatalf("sentinel text changed: %q", ErrIntentConflict.Error())
	}
	// The distinct start conflict keeps its own identity.
	if errors.Is(&APIError{HTTPStatus: 409, Code: "ONBOARDING_START_CONFLICT"}, ErrIntentConflict) {
		t.Fatal("distinct backend codes must not collapse")
	}
}

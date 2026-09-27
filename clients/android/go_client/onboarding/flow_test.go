package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

type scriptedIntent struct {
	mu       sync.Mutex
	calls    []string
	gateways []string
	step     func(n int, requestKey string) (IntentPoll, *APIError, error)
}

func (s *scriptedIntent) Intent(_ context.Context, requestKey, gatewayKey string) (IntentPoll, *APIError, error) {
	s.mu.Lock()
	s.calls = append(s.calls, requestKey)
	s.gateways = append(s.gateways, gatewayKey)
	n := len(s.calls)
	s.mu.Unlock()
	return s.step(n, requestKey)
}

func (s *scriptedIntent) sentGateways() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.gateways...)
}

func (s *scriptedIntent) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

type fakeStarter struct {
	mu     sync.Mutex
	calls  int
	reply  StartReply
	apiErr *APIError
	err    error
}

func (s *fakeStarter) Start(_ context.Context, _ IntentPoll) (StartReply, *APIError, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.reply, s.apiErr, s.err
}

func (s *fakeStarter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func testGateway() *Gateway {
	return &Gateway{NodeID: "node-4397f3b", Endpoint: GatewayEndpoint{
		PeerIP: "203.0.113.7", DTLSPort: 56002, DTLSSPKISHA256: testSPKI}}
}

func pendingPoll(key string) IntentPoll {
	return IntentPoll{State: StatePending, IntentID: testIntentID, RequestKey: key,
		ExpiresAt: time.Date(2026, 9, 23, 3, 0, 0, 0, time.UTC), RetryAfter: 1, Gateway: testGateway()}
}

func readyPoll(key string) IntentPoll {
	return IntentPoll{State: StateReady, IntentID: testIntentID, RequestKey: key,
		ExpiresAt: time.Date(2026, 9, 23, 2, 40, 0, 0, time.UTC), Gateway: testGateway(),
		CredentialID: "cred-1", Bootstrap: &Bootstrap{CredentialID: "cred-1", Secret: "c2VjcmV0"},
		StartChallenge: &StartChallenge{ChallengeID: "00112233445566778899aabbccddeeff",
			NonceB64: "bm9uY2U", ExpiresAt: "2026-09-23T02:10:00Z"}}
}

func startedPoll(key string) IntentPoll {
	return IntentPoll{State: StateStarted, IntentID: testIntentID, RequestKey: key,
		Gateway: testGateway(), CredentialID: "cred-1",
		StartedAt: time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC),
		NotAfter:  time.Date(2026, 9, 23, 3, 0, 0, 0, time.UTC)}
}

func newTestFlow(t *testing.T, intents IntentClient, starter Starter, opts Options) *Flow {
	t.Helper()
	if opts.Sleep == nil {
		opts.Sleep = func(context.Context, time.Duration) error { return nil }
	}
	flow, err := NewFlow(&MemoryStore{}, intents, starter, opts)
	if err != nil {
		t.Fatalf("flow: %v", err)
	}
	return flow
}

func TestFlowConnectReadyReusesKeyAcrossTaps(t *testing.T) {
	starter := &fakeStarter{}
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return readyPoll(key), nil, nil
	}}
	flow := newTestFlow(t, intents, starter, Options{})
	first, err := flow.Connect(context.Background())
	if err != nil || first.State != StateReady {
		t.Fatalf("first connect: %+v / %v", first, err)
	}
	second, err := flow.Connect(context.Background())
	if err != nil || second.State != StateReady {
		t.Fatalf("second connect: %+v / %v", second, err)
	}
	if intents.count() != 2 || intents.calls[0] == "" || intents.calls[0] != intents.calls[1] {
		t.Fatalf("repeated taps must reuse the same request_key: %v", intents.calls)
	}
	if starter.count() != 0 {
		t.Fatalf("Connect must never call the start RPC")
	}
}

func TestFlowPendingBudgetIsBounded(t *testing.T) {
	starter := &fakeStarter{}
	var sleeps []time.Duration
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		poll := pendingPoll(key)
		poll.RetryAfter = 3600
		return poll, nil, nil
	}}
	flow := newTestFlow(t, intents, starter, Options{MaxPendingPolls: 3, RetryAfterCap: 5 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }})
	result, err := flow.Connect(context.Background())
	if !errors.Is(err, ErrPendingBudget) || result.State != StatePending {
		t.Fatalf("bounded pending: %+v / %v", result, err)
	}
	if intents.count() != 3 {
		t.Fatalf("pending polls = %d, want 3", intents.count())
	}
	for _, slept := range sleeps {
		if slept > 5*time.Second {
			t.Fatalf("retry_after not clamped: %s", slept)
		}
	}
	if starter.count() != 0 {
		t.Fatalf("pending must not start the hour")
	}
}

func TestFlowCancelIsIdempotentAndKeepsKey(t *testing.T) {
	starter := &fakeStarter{}
	var flow *Flow
	var once sync.Once
	intents := &scriptedIntent{step: func(n int, key string) (IntentPoll, *APIError, error) {
		if n == 1 {
			once.Do(flow.Cancel)
			return pendingPoll(key), nil, nil
		}
		return readyPoll(key), nil, nil
	}}
	flow = newTestFlow(t, intents, starter, Options{})
	if _, err := flow.Connect(context.Background()); !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected cancel, got %v", err)
	}
	flow.Cancel()
	flow.Cancel()
	stored, present := flow.store.Load()
	if !present || stored.RequestKey == "" {
		t.Fatalf("cancel must keep the durable request_key")
	}
	result, err := flow.Connect(context.Background())
	if err != nil || result.State != StateReady {
		t.Fatalf("resume after cancel: %+v / %v", result, err)
	}
	if intents.calls[0] != intents.calls[len(intents.calls)-1] {
		t.Fatalf("cancel/retry must preserve the request_key")
	}
}

func TestFlowStartedRecoveryNeedsNoBootstrapSecret(t *testing.T) {
	starter := &fakeStarter{}
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return startedPoll(key), nil, nil
	}}
	flow := newTestFlow(t, intents, starter, Options{})
	first, err := flow.Connect(context.Background())
	if err != nil || first.State != StateStarted || !first.Recovered {
		t.Fatalf("started recovery: %+v / %v", first, err)
	}
	if first.Poll != nil && first.Poll.Bootstrap != nil {
		t.Fatalf("started recovery must not carry a bootstrap secret")
	}
	second, err := flow.Connect(context.Background())
	if err != nil || !second.Recovered || second.State != StateStarted {
		t.Fatalf("second started recovery: %+v / %v", second, err)
	}
	if intents.count() != 1 {
		t.Fatalf("recorded started must not re-poll: %d", intents.count())
	}
	if starter.count() != 0 {
		t.Fatalf("started recovery must not call the start RPC")
	}
}

func TestFlowStartRequiresReadyAndExplicitStarter(t *testing.T) {
	ready := readyPoll("unused")
	flows := []*Flow{
		newTestFlow(t, &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
			return readyPoll(key), nil, nil
		}}, nil, Options{}),
	}
	if _, err := flows[0].Start(context.Background(), ready); !errors.Is(err, ErrStartUnavailable) {
		t.Fatalf("nil starter must fail closed: %v", err)
	}
	starter := &fakeStarter{reply: StartReply{IntentID: testIntentID, CredentialID: "cred-1",
		StartedAt: time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC),
		NotAfter:  time.Date(2026, 9, 23, 3, 0, 0, 0, time.UTC)}}
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return readyPoll(key), nil, nil
	}}
	flow := newTestFlow(t, intents, starter, Options{})
	if _, err := flow.Start(context.Background(), readyPoll("other")); !errors.Is(err, ErrStartNotReady) {
		t.Fatalf("start before connect: %v", err)
	}
	if _, err := flow.Start(context.Background(), pendingPoll("other")); !errors.Is(err, ErrStartNotReady) {
		t.Fatalf("start from pending: %v", err)
	}
	result, err := flow.Connect(context.Background())
	if err != nil || result.Poll == nil {
		t.Fatalf("connect: %+v / %v", result, err)
	}
	mismatched := *result.Poll
	mismatched.RequestKey = "ffffffffffffffffffffffffffffffff"
	if _, err := flow.Start(context.Background(), mismatched); !errors.Is(err, ErrCorrelation) {
		t.Fatalf("correlation guard: %v", err)
	}
	reply, err := flow.Start(context.Background(), *result.Poll)
	if err != nil || !reply.StartedAt.Equal(starter.reply.StartedAt) {
		t.Fatalf("start: %+v / %v", reply, err)
	}
	if starter.count() != 1 {
		t.Fatalf("starter calls = %d, want 1", starter.count())
	}
	stored, _ := flow.store.Load()
	if stored.StartedAt == "" || stored.NotAfter == "" {
		t.Fatalf("start must be recorded durably: %+v", stored)
	}
	if _, err := flow.Start(context.Background(), *result.Poll); err != nil {
		t.Fatalf("same signed request retry must stay safe: %v", err)
	}
	if starter.count() != 2 {
		t.Fatalf("replay retry must reach the starter: %d", starter.count())
	}
}

func TestFlowRecoverNeverCreatesIntent(t *testing.T) {
	starter := &fakeStarter{}
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return pendingPoll(key), nil, nil
	}}
	flow := newTestFlow(t, intents, starter, Options{})
	result, err := flow.Recover(context.Background())
	if err != nil || result.State != StateNone || intents.count() != 0 {
		t.Fatalf("recover without key must not create an intent: %+v / %v", result, err)
	}
	if _, err := flow.Connect(context.Background()); !errors.Is(err, ErrPendingBudget) {
		t.Fatalf("connect should only budget-poll: %v", err)
	}
	result, err = flow.Recover(context.Background())
	if err != nil || result.State != StatePending || result.Recovered {
		t.Fatalf("recover with a key: %+v / %v", result, err)
	}
}

func TestFlowTerminalStopsNewIntents(t *testing.T) {
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return IntentPoll{State: StateFailed, IntentID: testIntentID, RequestKey: key,
			FailureReason: "GATEWAY_UNAVAILABLE"}, nil, nil
	}}
	flow := newTestFlow(t, intents, &fakeStarter{}, Options{})
	result, err := flow.Connect(context.Background())
	if err != nil || result.State != StateFailed {
		t.Fatalf("failed intent: %+v / %v", result, err)
	}
	if _, err := flow.Connect(context.Background()); err != nil {
		t.Fatalf("terminal repeat: %v", err)
	}
	if intents.count() != 1 {
		t.Fatalf("terminal must not create a new intent: %d", intents.count())
	}

	expiredStarter := &fakeStarter{}
	expiredIntents := &scriptedIntent{step: func(n int, key string) (IntentPoll, *APIError, error) {
		if n == 1 {
			return IntentPoll{}, &APIError{HTTPStatus: 410, Code: "ONBOARDING_INTENT_EXPIRED"}, nil
		}
		return readyPoll(key), nil, nil
	}}
	expiredFlow := newTestFlow(t, expiredIntents, expiredStarter, Options{})
	if _, err := expiredFlow.Connect(context.Background()); !errors.Is(err, ErrIntentExpired) {
		t.Fatalf("expired code must stay distinct: %v", err)
	}
	// A NEW explicit Connect supersedes a server-expired attempt that never started
	// its hour: exactly one fresh intent with a rotated request_key, never a silent
	// no-op and never a background restart.
	repeat, err := expiredFlow.Connect(context.Background())
	if err != nil || repeat.State != StateReady {
		t.Fatalf("expired recovery must create a fresh ready attempt: %+v / %v", repeat, err)
	}
	if expiredIntents.count() != 2 {
		t.Fatalf("expired recovery calls = %d, want 2", expiredIntents.count())
	}
	if expiredIntents.calls[0] == "" || expiredIntents.calls[0] == expiredIntents.calls[1] {
		t.Fatalf("expired recovery must rotate the request_key: %v", expiredIntents.calls)
	}
	if expiredStarter.count() != 0 {
		t.Fatalf("expired recovery must not start the hour")
	}
}

// TestFlowExpiredTerminalRecoversWithFreshKeyAndInstallation proves a NEW explicit
// Connect after a persisted server-expired attempt without any started hour resets
// only the attempt identity: exactly one new intent with a fresh request_key, while
// the durable installation/environment binding is preserved.
func TestFlowExpiredTerminalRecoversWithFreshKeyAndInstallation(t *testing.T) {
	var persisted [][]byte
	persist := func(_ context.Context, payload []byte) error {
		persisted = append(persisted, append([]byte(nil), payload...))
		return nil
	}
	seed, err := json.Marshal(flowEnvelope{Version: 1, InstallationID: "inst-1", Environment: "test",
		State: KeyState{RequestKey: "expired-key", IntentID: testIntentID, Terminal: string(StateExpired)}})
	if err != nil {
		t.Fatal(err)
	}
	store := NewDurableStore(seed, "inst-1", "test", persist)
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return readyPoll(key), nil, nil
	}}
	flow, err := NewFlow(store, intents, &fakeStarter{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if state := flow.State(); state != StateExpired {
		t.Fatalf("seeded state = %s, want expired", state)
	}
	result, err := flow.Connect(context.Background())
	if err != nil || result.State != StateReady {
		t.Fatalf("expired recovery connect: %+v / %v", result, err)
	}
	if intents.count() != 1 || intents.calls[0] == "" || intents.calls[0] == "expired-key" {
		t.Fatalf("expired recovery must create exactly one intent with a fresh key: %v", intents.calls)
	}
	if len(persisted) == 0 {
		t.Fatal("the fresh request_key must be persisted before the network call")
	}
	var first flowEnvelope
	if err := json.Unmarshal(persisted[0], &first); err != nil {
		t.Fatal(err)
	}
	if first.State.RequestKey != intents.calls[0] || first.State.Terminal != "" {
		t.Fatalf("fresh request_key must be durable before the network call: %+v", first.State)
	}
	var last flowEnvelope
	if err := json.Unmarshal(persisted[len(persisted)-1], &last); err != nil {
		t.Fatal(err)
	}
	if last.InstallationID != "inst-1" || last.Environment != "test" {
		t.Fatalf("installation binding must be preserved: %+v", last)
	}
	if last.State.RequestKey != intents.calls[0] || last.State.Terminal != "" {
		t.Fatalf("persisted recovery state wrong: %+v", last.State)
	}
}

// TestFlowExpiredTerminalWithStartedAtStaysFailClosed proves the started/hour guard:
// a persisted expiry that already carries a started identity is never erased.
func TestFlowExpiredTerminalWithStartedAtStaysFailClosed(t *testing.T) {
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return readyPoll(key), nil, nil
	}}
	flow := newTestFlow(t, intents, &fakeStarter{}, Options{})
	started := time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if err := flow.store.Save(KeyState{RequestKey: "key-1", IntentID: testIntentID,
		Terminal: string(StateExpired), StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	result, err := flow.Connect(context.Background())
	if err != nil || result.State != StateExpired {
		t.Fatalf("started expired identity must stay terminal: %+v / %v", result, err)
	}
	if intents.count() != 0 {
		t.Fatalf("started expired identity must make no network call: %d", intents.count())
	}
	stored, _ := flow.store.Load()
	if stored.RequestKey != "key-1" || stored.StartedAt != started {
		t.Fatalf("started identity must be preserved: %+v", stored)
	}
}

// TestFlowRevokedTerminalStaysFailClosed proves only the expired kind recovers:
// a revoked terminal keeps answering with the stored terminal state and no network.
func TestFlowRevokedTerminalStaysFailClosed(t *testing.T) {
	intents := &scriptedIntent{step: func(_ int, _ string) (IntentPoll, *APIError, error) {
		return IntentPoll{}, &APIError{HTTPStatus: 403, Code: "ONBOARDING_INTENT_REVOKED"}, nil
	}}
	flow := newTestFlow(t, intents, &fakeStarter{}, Options{})
	first, err := flow.Connect(context.Background())
	if !errors.Is(err, ErrIntentRevoked) || first.State != StateRevoked {
		t.Fatalf("revoked intent: %+v / %v", first, err)
	}
	repeat, err := flow.Connect(context.Background())
	if err != nil || repeat.State != StateRevoked {
		t.Fatalf("revoked terminal must stay fail-closed: %+v / %v", repeat, err)
	}
	if intents.count() != 1 {
		t.Fatalf("revoked terminal must not create a new intent: %d", intents.count())
	}
}

// TestFlowExpiredRecoveryPersistFailureKeepsIdentity proves the fail-closed save
// order: when the fresh request_key cannot be persisted, zero requests are made and
// the previous durable identity (expired attempt) stays in force.
func TestFlowExpiredRecoveryPersistFailureKeepsIdentity(t *testing.T) {
	seed, err := json.Marshal(flowEnvelope{Version: 1, InstallationID: "inst-1", Environment: "test",
		State: KeyState{RequestKey: "expired-key", IntentID: testIntentID, Terminal: string(StateExpired)}})
	if err != nil {
		t.Fatal(err)
	}
	store := NewDurableStore(seed, "inst-1", "test", func(context.Context, []byte) error {
		return errors.New("PERSIST_UNAVAILABLE")
	})
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return readyPoll(key), nil, nil
	}}
	flow, err := NewFlow(store, intents, &fakeStarter{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Connect(context.Background()); err == nil {
		t.Fatal("persist failure must surface before any network call")
	}
	if intents.count() != 0 {
		t.Fatalf("failed persist must make zero requests: %d", intents.count())
	}
	stored, ok := store.Load()
	if !ok || stored.RequestKey != "expired-key" || stored.Terminal != string(StateExpired) {
		t.Fatalf("previous durable identity must stay in force: %+v %v", stored, ok)
	}
}

// TestFlowRecoverNeverRevivesExpiredTerminal proves the background/recover path is
// unchanged: it reports the stored terminal and never creates, polls or resets it.
func TestFlowRecoverNeverRevivesExpiredTerminal(t *testing.T) {
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return readyPoll(key), nil, nil
	}}
	flow := newTestFlow(t, intents, &fakeStarter{}, Options{})
	if err := flow.store.Save(KeyState{RequestKey: "expired-key", IntentID: testIntentID,
		Terminal: string(StateExpired)}); err != nil {
		t.Fatal(err)
	}
	result, err := flow.Recover(context.Background())
	if err != nil || result.State != StateExpired || result.Recovered {
		t.Fatalf("recover must report the stored terminal, not revive it: %+v / %v", result, err)
	}
	if intents.count() != 0 {
		t.Fatalf("recover must never create or poll an intent: %d", intents.count())
	}
	if state := flow.State(); state != StateExpired {
		t.Fatalf("recover must not rewrite the durable terminal: %s", state)
	}
}

func TestFlowInFlightAndCancelDuringPending(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return pendingPoll(key), nil, nil
	}}
	flow := newTestFlow(t, intents, &fakeStarter{}, Options{MaxPendingPolls: 1})
	done := make(chan error, 1)
	go func() {
		_, err := flow.Connect(context.Background())
		done <- err
	}()
	<-entered
	if _, err := flow.Connect(context.Background()); !errors.Is(err, ErrInFlight) {
		t.Fatalf("second concurrent connect must be rejected: %v", err)
	}
	flow.Cancel()
	close(release)
	if err := <-done; !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancel during pending must abort the call: %v", err)
	}
}

func TestFlowPendingRetryOneSecondReachesReadyUnderBudget(t *testing.T) {
	starter := &fakeStarter{reply: StartReply{IntentID: testIntentID, CredentialID: "cred-1",
		StartedAt: time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC),
		NotAfter:  time.Date(2026, 9, 23, 3, 0, 0, 0, time.UTC)}}
	var sleeps []time.Duration
	intents := &scriptedIntent{step: func(step int, key string) (IntentPoll, *APIError, error) {
		if step < 2 {
			return pendingPoll(key), nil, nil
		}
		return readyPoll(key), nil, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	flow := newTestFlow(t, intents, starter, Options{
		Sleep: func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil },
	})
	result, err := flow.Connect(ctx)
	if err != nil || result.State != StateReady {
		t.Fatalf("pending retry=1s must reach ready inside the explicit budget: %+v / %v", result, err)
	}
	if intents.count() != 2 {
		t.Fatalf("pending polls = %d, want 2 (pending then ready)", intents.count())
	}
	if len(sleeps) != 1 || sleeps[0] != time.Second {
		t.Fatalf("retry_after=1s must sleep exactly once for 1s, got %v", sleeps)
	}
	key := intents.calls[0]
	if key == "" {
		t.Fatal("request_key must be set")
	}
	for _, called := range intents.calls {
		if called != key {
			t.Fatalf("polls must reuse the same request_key: %v", intents.calls)
		}
	}
	if starter.count() != 0 {
		t.Fatal("Connect must never start the hour in the background")
	}
	if _, err := flow.Start(ctx, readyPoll(key)); err != nil {
		t.Fatalf("explicit Start after ready: %v", err)
	}
	if starter.count() != 1 {
		t.Fatalf("explicit Start must call the starter exactly once, got %d", starter.count())
	}
}

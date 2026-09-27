package onboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestDurableStoreBindsInstallationAndEnvironment proves a restored blob is only
// resumed for the same installation fingerprint and environment; a foreign or
// mismatched file yields no identity instead of silently resuming another attempt.
func TestDurableStoreBindsInstallationAndEnvironment(t *testing.T) {
	persist := func(context.Context, []byte) error { return nil }
	seed, err := json.Marshal(flowEnvelope{Version: 1, InstallationID: "inst-1", Environment: "test",
		State: KeyState{RequestKey: "key-1", IntentID: testIntentID}})
	if err != nil {
		t.Fatal(err)
	}
	if state, ok := NewDurableStore(seed, "inst-1", "test", persist).Load(); !ok || state.RequestKey != "key-1" {
		t.Fatalf("matching binding must load: %+v %v", state, ok)
	}
	if _, ok := NewDurableStore(seed, "inst-2", "test", persist).Load(); ok {
		t.Fatal("foreign installation must not resume")
	}
	if _, ok := NewDurableStore(seed, "inst-1", "prod", persist).Load(); ok {
		t.Fatal("foreign environment must not resume")
	}
	if _, ok := NewDurableStore([]byte("{"), "inst-1", "test", persist).Load(); ok {
		t.Fatal("malformed seed must not resume")
	}
}

// TestFlowPersistFailureBlocksIntent proves the request_key is persisted before the
// first network call: a failing durable write yields no intent call and no new key.
func TestFlowPersistFailureBlocksIntent(t *testing.T) {
	store := NewDurableStore(nil, "inst-1", "test", func(context.Context, []byte) error {
		return errors.New("PERSIST_UNAVAILABLE")
	})
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return pendingPoll(key), nil, nil
	}}
	flow, err := NewFlow(store, intents, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Connect(context.Background()); err == nil {
		t.Fatal("persist failure must surface")
	}
	if intents.count() != 0 {
		t.Fatalf("no network attempt after a failed persist: %d", intents.count())
	}
	if _, ok := store.Load(); ok {
		t.Fatal("a failed persist must not keep an in-memory key")
	}
}

// TestDurableStoreRestartReusesRequestKey proves cancel/restart keeps the identity:
// a new Flow over the persisted blob reuses the same request_key for the retry.
func TestDurableStoreRestartReusesRequestKey(t *testing.T) {
	var persisted []byte
	persist := func(_ context.Context, payload []byte) error {
		persisted = append([]byte(nil), payload...)
		return nil
	}
	first := NewDurableStore(nil, "inst-1", "test", persist)
	calls := []string{}
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		calls = append(calls, key)
		return pendingPoll(key), nil, nil
	}}
	flow, err := NewFlow(first, intents, nil, Options{MaxPendingPolls: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := flow.Connect(context.Background())
	if !errors.Is(err, ErrPendingBudget) || result.State != StatePending {
		t.Fatalf("first bounded connect: %+v / %v", result, err)
	}
	flow.Cancel()
	if len(calls) != 1 {
		t.Fatalf("one intent call expected: %v", calls)
	}

	restarted := NewDurableStore(persisted, "inst-1", "test", persist)
	secondIntents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		calls = append(calls, key)
		return readyPoll(key), nil, nil
	}}
	second, err := NewFlow(restarted, secondIntents, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := second.Connect(context.Background())
	if err != nil || ready.State != StateReady {
		t.Fatalf("restarted connect: %+v / %v", ready, err)
	}
	if len(calls) != 2 || calls[0] == "" || calls[0] != calls[1] {
		t.Fatalf("restart must reuse the persisted request_key: %v", calls)
	}
	if state, ok := restarted.Load(); !ok || state.IntentID != testIntentID || state.RequestKey != calls[1] {
		t.Fatalf("ready identity not durable: %+v %v", state, ok)
	}
}

// TestFlowDurableStartPersistsOnlyIdentity proves the start path stores no bootstrap
// secret: the persisted envelope carries request_key/intent/credential metadata only.
func TestFlowDurableStartPersistsOnlyIdentity(t *testing.T) {
	var persisted [][]byte
	persist := func(_ context.Context, payload []byte) error {
		persisted = append(persisted, append([]byte(nil), payload...))
		return nil
	}
	store := NewDurableStore(nil, "inst-1", "test", persist)
	intents := &scriptedIntent{step: func(_ int, key string) (IntentPoll, *APIError, error) {
		return readyPoll(key), nil, nil
	}}
	starter := &fakeStarter{reply: StartReply{IntentID: testIntentID, CredentialID: "cred-1",
		StartedAt: time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC),
		NotAfter:  time.Date(2026, 9, 23, 3, 0, 0, 0, time.UTC)}}
	flow, err := NewFlow(store, intents, starter, Options{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := flow.Connect(context.Background())
	if err != nil || result.State != StateReady || result.Poll == nil {
		t.Fatalf("connect: %+v / %v", result, err)
	}
	if _, err := flow.Start(context.Background(), *result.Poll); err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(persisted) == 0 {
		t.Fatal("start must persist the recorded hour")
	}
	for _, blob := range persisted {
		if bytes.Contains(blob, []byte("ready-secret")) || bytes.Contains(blob, []byte("c2VjcmV0")) {
			t.Fatal("bootstrap secret must never be persisted")
		}
	}
	var final flowEnvelope
	if err := json.Unmarshal(persisted[len(persisted)-1], &final); err != nil {
		t.Fatal(err)
	}
	if final.State.StartedAt == "" || final.State.CredentialID != "cred-1" {
		t.Fatalf("final durable state: %+v", final.State)
	}
}

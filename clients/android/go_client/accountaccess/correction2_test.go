package accountaccess

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// correction2 (review review-034core-cor1-20260921/REPORT.md R1/R2): poll advances a
// pending operation durably, and /me+catalog+admission are fenced by subject/session.

func operationBody(operationID, accessApplicationState string) string {
	state := "applied"
	if accessApplicationState != "applied" && accessApplicationState != "rejected" {
		state = "applying"
	}
	return fmt.Sprintf(`{"request_id":"0123456789abcdef0123456789abcdef",
		"server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"ok",
		"operation_id":%q,"operation_type":"access_sync","state":%q,
		"access_application_state":%q,"per_node":[]}`, operationID, state, accessApplicationState)
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func pendingOperation(t *testing.T, fake *fakeServer, coordinator *Coordinator, binding string) {
	t.Helper()
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("7", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.syncResponse = syncBody("op-1", "pending")
	result, err := coordinator.SyncAccess(context.Background())
	if err != nil || result.Receipt == nil {
		t.Fatalf("pending sync: %+v %v", result, err)
	}
}

func TestCorrection2PollAdvancesPendingToTerminalAndAllowsNewOperation(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, store := newTestCoordinator(t, fake, &subject)
	binding := "1"
	pendingOperation(t, fake, coordinator, binding)
	firstKey := fake.lastSyncKey

	fake.operationBody = operationBody("op-1", "applied")
	poll, err := coordinator.PollAccessOperation(context.Background())
	if err != nil || !poll.Terminal || poll.Receipt == nil {
		t.Fatalf("poll: %+v %v", poll, err)
	}
	if poll.Receipt.Response.AccessApplicationState != "applied" {
		t.Fatalf("mapped state wrong: %+v", poll.Receipt.Response)
	}
	if store.Value == nil || store.Value.Response == nil ||
		store.Value.Response.AccessApplicationState != "applied" {
		t.Fatalf("terminal state must be durably persisted: %+v", store.Value)
	}

	replay, err := coordinator.SyncAccess(context.Background())
	if err != nil || !replay.Replayed || fake.syncCalls != 1 {
		t.Fatalf("terminal same body must replay without a new effect: %+v calls=%d %v", replay, fake.syncCalls, err)
	}

	fake.gatewayBody = catalogBody("9", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.SyncAccess(context.Background())
	if err != nil || second.Receipt == nil || fake.syncCalls != 2 || fake.lastSyncKey == firstKey {
		t.Fatalf("terminal receipt must free a new operation: %+v calls=%d key=%q %v",
			second, fake.syncCalls, fake.lastSyncKey, err)
	}
}

type failingSaveStore struct {
	inner MemoryReceiptStore
	fail  bool
}

func (f *failingSaveStore) Load() (*Receipt, error) { return f.inner.Load() }
func (f *failingSaveStore) Save(value Receipt) error {
	if f.fail {
		return fmt.Errorf("store unavailable")
	}
	return f.inner.Save(value)
}

func TestCorrection2PollSaveFailureKeepsPending(t *testing.T) {
	fake := &fakeServer{}
	server := httptest.NewServer(fake.handler())
	defer server.Close()
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	store := &failingSaveStore{}
	coordinator, err := NewCoordinator(Options{
		Client:    &Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: StaticToken("bearer")},
		AttemptID: "attempt",
		Store:     store,
		Subject:   func() Subject { return subject },
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := "1"
	pendingOperation(t, fake, coordinator, binding)
	store.fail = true
	fake.operationBody = operationBody("op-1", "applied")
	if _, err := coordinator.PollAccessOperation(context.Background()); err == nil {
		t.Fatal("failed save must surface an error")
	}
	if coordinator.receipt == nil || coordinator.receipt.Response.AccessApplicationState != "pending" {
		t.Fatalf("failed save must not advance the pending operation: %+v", coordinator.receipt)
	}
	if store.inner.Value == nil || store.inner.Value.Response.AccessApplicationState != "pending" {
		t.Fatalf("failed save must not overwrite the durable pending receipt: %+v", store.inner.Value)
	}
	replay, err := coordinator.SyncAccess(context.Background())
	if err != nil || !replay.Replayed || fake.syncCalls != 1 {
		t.Fatalf("pending must stay effect-once after a failed save: %+v calls=%d %v", replay, fake.syncCalls, err)
	}
	store.fail = false
	poll, err := coordinator.PollAccessOperation(context.Background())
	if err != nil || !poll.Terminal {
		t.Fatalf("retry poll after save recovery: %+v %v", poll, err)
	}
}

func TestCorrection2RestartAfterTerminalPollReplaysThenNewOperation(t *testing.T) {
	fake := &fakeServer{}
	server := httptest.NewServer(fake.handler())
	defer server.Close()
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	store := &MemoryReceiptStore{}
	options := Options{
		Client:    &Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: StaticToken("bearer")},
		AttemptID: "attempt",
		Store:     store,
		Subject:   func() Subject { return subject },
	}
	coordinator, err := NewCoordinator(options)
	if err != nil {
		t.Fatal(err)
	}
	binding := "1"
	pendingOperation(t, fake, coordinator, binding)
	fake.operationBody = operationBody("op-1", "applied")
	if _, err := coordinator.PollAccessOperation(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := fake.lastSyncKey

	restarted, err := NewCoordinator(options)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := restarted.SyncAccess(context.Background())
	if err != nil || !replay.Replayed || fake.syncCalls != 1 || fake.lastSyncKey != key {
		t.Fatalf("restart after poll must replay the terminal operation without /me: %+v calls=%d key=%q %v",
			replay, fake.syncCalls, fake.lastSyncKey, err)
	}

	// A genuinely new admissible body still requires fresh /me + catalog fences.
	fake.mu.Lock()
	fake.meBody = meBody("8", &binding, "active")
	fake.mu.Unlock()
	if _, err := restarted.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("9", 1)
	if _, err := restarted.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.syncResponse = syncBody("op-2", "applied")
	second, err := restarted.SyncAccess(context.Background())
	if err != nil || second.Receipt == nil || fake.syncCalls != 2 || fake.lastSyncKey == key {
		t.Fatalf("new body after restart must start a new operation: %+v calls=%d %v", second, fake.syncCalls, err)
	}
}

func TestCorrection2LatePollDoesNotChangeNewOperation(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, store := newTestCoordinator(t, fake, &subject)
	binding := "1"
	pendingOperation(t, fake, coordinator, binding)
	oldKey := fake.lastSyncKey

	// Block the old operation's poll response, replace the operation, then release.
	block := make(chan struct{})
	fake.mu.Lock()
	fake.operationBlock = block
	fake.operationBody = operationBody("op-1", "applied")
	fake.mu.Unlock()
	polled := make(chan PollResult, 1)
	go func() {
		result, _ := coordinator.PollAccessOperation(context.Background())
		polled <- result
	}()
	waitFor(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.operationCalls >= 1
	})

	subject = Subject{AccountRef: "acc-2", InstallationID: "inst-1"}
	bindingTwo := "2"
	fake.mu.Lock()
	fake.operationBlock = nil
	fake.meBody = meBody("8", &bindingTwo, "active")
	fake.mu.Unlock()
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("5", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.syncResponse = syncBody("op-2", "applied")
	replaced, err := coordinator.SyncAccess(context.Background())
	if err != nil || replaced.Receipt == nil || replaced.Receipt.Response.OperationID != "op-2" {
		t.Fatalf("new subject operation: %+v %v", replaced, err)
	}
	newKey := fake.lastSyncKey
	if newKey == oldKey {
		t.Fatal("new subject must use a new key")
	}

	close(block)
	late := <-polled
	if !late.Stale {
		t.Fatalf("late poll of a replaced operation must not apply: %+v", late)
	}
	if coordinator.receipt == nil || coordinator.receipt.Response.OperationID != "op-2" ||
		coordinator.receipt.Response.AccessApplicationState != "applied" {
		t.Fatalf("late poll changed the new operation: %+v", coordinator.receipt)
	}
	if store.Value == nil || store.Value.Response.OperationID != "op-2" {
		t.Fatalf("late poll changed the durable receipt: %+v", store.Value)
	}
}

func TestCorrection2SubjectChangeRequiresFreshMeZeroPost(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("7", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}

	subject = Subject{AccountRef: "acc-2", InstallationID: "inst-1"}
	if _, err := coordinator.SyncAccess(context.Background()); err != ErrMeRequired {
		t.Fatalf("foreign subject revisions must not be POSTed: %v", err)
	}
	if fake.syncCalls != 0 {
		t.Fatalf("zero POST expected before fresh /me, got %d", fake.syncCalls)
	}
	fake.syncResponse = syncBody("op-2", "applied")
	fake.meBody = meBody("8", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("8", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.SyncAccess(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.syncCalls != 1 {
		t.Fatalf("fresh subject sync calls=%d", fake.syncCalls)
	}
}

func TestCorrection2InflightMeResponseNotAppliedToNewSubject(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	block := make(chan struct{})
	fake.mu.Lock()
	fake.meBlock = block
	fake.mu.Unlock()
	refreshed := make(chan Result, 1)
	go func() {
		result, _ := coordinator.Refresh(context.Background())
		refreshed <- result
	}()
	waitFor(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.meCalls >= 1
	})
	subject = Subject{AccountRef: "acc-2", InstallationID: "inst-1"}
	fake.mu.Lock()
	fake.meBlock = nil
	fake.mu.Unlock()
	close(block)
	result := <-refreshed
	if !result.Dropped || result.Projection != nil {
		t.Fatalf("in-flight /me of the old subject must be dropped: %+v", result)
	}
}

func TestCorrection2PollWithoutPendingIsNoop(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	poll, err := coordinator.PollAccessOperation(context.Background())
	if err != nil || poll.State != StateIdle || fake.operationCalls != 0 {
		t.Fatalf("poll without pending must be a no-op: %+v calls=%d %v", poll, fake.operationCalls, err)
	}
}

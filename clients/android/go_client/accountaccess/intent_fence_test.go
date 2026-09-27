package accountaccess

// S5 intent fence (client side, exact wire alignment): the client polls
// GET /operations/{id}?observe=sync_recovery and the ONLY rotation trigger is the exact
// additive marker {required, catalog_revision, binding_revision}. Before a generic
// retryable_failure/not_requested receipt is resent, SyncAccess itself issues one
// read-only opt-in probe of that operation, so the marker can be observed without a
// separate pending-path poll; a probe failure falls back to exactly the persisted resend.
// While the marker is absent or required=false, a retryable_failure/not_requested intent
// keeps resending exactly its persisted key/body and is probed again on every invocation
// (no "already probed" cache). A required marker with BOTH revisions valid mints
// exactly one new key carrying exactly the marker revisions, persisted before the POST; a
// required marker without a usable pair returns a bounded error without any POST and
// without resending the stale body. Hermetic httptest only: no live network, no server
// change, no self-authorized rotation, no capability header.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fenceAttempt is one observed POST /access/sync: the wire key/body, a copy of the
// durable receipt exactly at send time, and whether the response was cut.
type fenceAttempt struct {
	key     string
	body    string
	receipt *Receipt
	cut     bool
}

// fenceObserver wraps the synthetic contract server: it records the exact raw
// /operations read URI, snapshots the durable receipt at POST time (before the response
// is written) and can cut one response to simulate a lost response, without touching the
// shared fakeServer fixtures.
type fenceObserver struct {
	inner http.Handler
	store *MemoryReceiptStore

	mu        sync.Mutex
	attempts  []fenceAttempt
	reads     []string
	cutNext   bool
	syncDelay time.Duration
}

func (o *fenceObserver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/operations/") {
		o.mu.Lock()
		o.reads = append(o.reads, request.Method+" "+request.URL.RequestURI())
		o.mu.Unlock()
	}
	if request.Method == http.MethodPost && request.URL.Path == "/access/sync" {
		raw, _ := io.ReadAll(request.Body)
		o.mu.Lock()
		attempt := fenceAttempt{key: request.Header.Get("Idempotency-Key"), body: string(raw)}
		if o.store.Value != nil {
			copied := *o.store.Value
			attempt.receipt = &copied
		}
		attempt.cut = o.cutNext
		o.cutNext = false
		delay := o.syncDelay
		o.attempts = append(o.attempts, attempt)
		o.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if attempt.cut {
			// Advertise more bytes than are sent: the client observes a transport
			// error after the request already reached the server.
			writer.Header().Set("Content-Length", "4096")
			_, _ = io.WriteString(writer, `{"request_id":"0123456789abcdef0123456789abcdef",`)
			return
		}
	}
	o.inner.ServeHTTP(writer, request)
}

func (o *fenceObserver) snapshot() []fenceAttempt {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]fenceAttempt(nil), o.attempts...)
}

func (o *fenceObserver) readSnapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.reads...)
}

func (o *fenceObserver) cutNextAttempt() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cutNext = true
}

func (o *fenceObserver) setSyncDelay(delay time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.syncDelay = delay
}

// fenceOperationBody is the GET /operations/{id} body. A non-empty syncRecovery is
// inserted verbatim so the exact marker (or a malformed variant) is exercised on the
// wire; an empty one produces the legacy shape with no sync_recovery field at all.
func fenceOperationBody(operationID, accessApplicationState, perNode, syncRecovery string) string {
	if perNode == "" {
		perNode = "[]"
	}
	extra := ""
	if syncRecovery != "" {
		extra = fmt.Sprintf(`,"sync_recovery":%s`, syncRecovery)
	}
	state := "applied"
	if accessApplicationState != "applied" && accessApplicationState != "rejected" {
		state = "applying"
	}
	return fmt.Sprintf(`{"request_id":"0123456789abcdef0123456789abcdef",
		"server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"ok",
		"operation_id":%q,"operation_type":"access_sync","state":%q,
		"access_application_state":%q,"per_node":%s%s}`,
		operationID, state, accessApplicationState, perNode, extra)
}

type intentFenceFixture struct {
	fake        *fakeServer
	observer    *fenceObserver
	coordinator *Coordinator
	store       *MemoryReceiptStore
	options     Options
	subject     *Subject
}

func newIntentFenceFixture(t *testing.T, subject *Subject) *intentFenceFixture {
	t.Helper()
	fake := &fakeServer{}
	store := &MemoryReceiptStore{}
	observer := &fenceObserver{inner: fake.handler(), store: store}
	server := httptest.NewServer(observer)
	t.Cleanup(server.Close)
	options := Options{
		Client:    &Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: StaticToken("test-bearer")},
		AttemptID: "attempt",
		Store:     store,
		Subject:   func() Subject { return *subject },
	}
	coordinator, err := NewCoordinator(options)
	if err != nil {
		t.Fatal(err)
	}
	return &intentFenceFixture{fake: fake, observer: observer, coordinator: coordinator,
		store: store, options: options, subject: subject}
}

func (f *intentFenceFixture) restart(t *testing.T) *Coordinator {
	t.Helper()
	restarted, err := NewCoordinator(f.options)
	if err != nil {
		t.Fatal(err)
	}
	return restarted
}

func (f *intentFenceFixture) refreshMe(t *testing.T, revision string, binding *string, onboarding string) {
	t.Helper()
	f.fake.meBody = meBody(revision, binding, onboarding)
	if _, err := f.coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *intentFenceFixture) refreshCatalog(t *testing.T, revision string) {
	t.Helper()
	f.fake.gatewayBody = catalogBody(revision, 1)
	if _, err := f.coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// firstSync creates the accepted handoff state: a persisted 19/1 receipt whose response
// is retryable_failure. It returns the first durable key.
func (f *intentFenceFixture) firstSync(t *testing.T, operationID string) string {
	t.Helper()
	f.refreshMe(t, "7", strPtr("1"), "active")
	f.refreshCatalog(t, "19")
	f.fake.syncResponse = syncBody(operationID, "retryable_failure")
	first, err := f.coordinator.SyncAccess(context.Background())
	if err != nil || first.Receipt == nil {
		t.Fatalf("first sync: %+v %v", first, err)
	}
	return first.Receipt.Key
}

// pollMarker drives one GET /operations poll with the exact marker body and returns the
// durable receipt after the poll.
func (f *intentFenceFixture) pollMarker(t *testing.T, operationID, aggregateState, marker string) *Receipt {
	t.Helper()
	f.fake.operationBody = fenceOperationBody(operationID, aggregateState, "", marker)
	poll, err := f.coordinator.PollAccessOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if poll.Receipt == nil {
		t.Fatalf("poll produced no receipt: %+v", poll)
	}
	return poll.Receipt
}

// assertKeyBodyPairing is the invariant that one key is never sent with two bodies.
func assertKeyBodyPairing(t *testing.T, attempts []fenceAttempt) {
	t.Helper()
	pairs := map[string]string{}
	for _, attempt := range attempts {
		if body, seen := pairs[attempt.key]; seen && body != attempt.body {
			t.Fatalf("key %s was sent with two different bodies:\n%s\n%s", attempt.key, body, attempt.body)
		}
		pairs[attempt.key] = attempt.body
	}
}

func distinctKeys(attempts []fenceAttempt) map[string]bool {
	keys := map[string]bool{}
	for _, attempt := range attempts {
		keys[attempt.key] = true
	}
	return keys
}

// TestIntentFenceAObserveQueryIsExact is the (a) case: both PollAccessOperation and
// PollOperation read GET /operations/{id}?observe=sync_recovery with the exact raw query.
func TestIntentFenceAObserveQueryIsExact(t *testing.T) {
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	f := newIntentFenceFixture(t, &subject)
	f.firstSync(t, "op-1")
	f.fake.operationBody = fenceOperationBody("op-1", "retryable_failure", "", "")

	if _, err := f.coordinator.PollAccessOperation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.coordinator.PollOperation(context.Background(), "op-1"); err != nil {
		t.Fatal(err)
	}
	reads := f.observer.readSnapshot()
	if len(reads) != 2 {
		t.Fatalf("operation reads=%d want 2: %v", len(reads), reads)
	}
	for _, read := range reads {
		if read != "GET /operations/op-1?observe=sync_recovery" {
			t.Fatalf("operation read %q must carry the exact observe=sync_recovery query", read)
		}
	}
}

// TestIntentFenceBLegacyShapeNoMarkerResendsAndStrictRejects is the (b) case: a legacy
// response without sync_recovery decodes and never rotates on retryable_failure, while an
// unknown top-level field (or an unknown nested marker field) is still strict-rejected.
func TestIntentFenceBLegacyShapeNoMarkerResendsAndStrictRejects(t *testing.T) {
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	f := newIntentFenceFixture(t, &subject)
	originalKey := f.firstSync(t, "op-1")
	legacy := fenceOperationBody("op-1", "retryable_failure", "", "")
	f.fake.operationBody = legacy

	receipt := f.pollMarker(t, "op-1", "retryable_failure", "")
	if receipt.RecoveryRequired || receipt.RecoveryRevisions != nil {
		t.Fatalf("legacy response must not mark recovery: %+v", receipt)
	}
	var decoded OperationResponse
	if err := decodeStrict([]byte(legacy), &decoded); err != nil || decoded.SyncRecovery != nil {
		t.Fatalf("legacy response must decode unchanged with no marker: %+v %v", decoded, err)
	}
	unknownTop := strings.Replace(legacy, `"per_node":[]`, `"per_node":[],"mystery":true`, 1)
	if err := decodeStrict([]byte(unknownTop), &OperationResponse{}); err == nil {
		t.Fatal("unknown top-level field must still be rejected")
	}
	unknownNested := fenceOperationBody("op-1", "retryable_failure", "",
		`{"required":true,"catalog_revision":"20","binding_revision":"1","mystery":1}`)
	if err := decodeStrict([]byte(unknownNested), &OperationResponse{}); err == nil {
		t.Fatal("unknown nested marker field must still be rejected")
	}

	f.refreshCatalog(t, "20")
	result, err := f.coordinator.SyncAccess(context.Background())
	if err != nil || result.Receipt == nil {
		t.Fatalf("generic retryable retry: %+v %v", result, err)
	}
	attempts := f.observer.snapshot()
	if len(attempts) != 2 || attempts[1].key != originalKey {
		t.Fatalf("unmarked retryable must not rotate: %+v", attempts)
	}
	if attempts[1].body != attempts[0].body {
		t.Fatalf("unmarked retryable must resend the persisted body: %s", attempts[1].body)
	}
	assertKeyBodyPairing(t, attempts)
}

// TestIntentFenceCRotationUsesCurrentMarkerRevisionsAndResumesOneKey is the (c) case:
// saved 19/1 + marker 20/1 rotates exactly once with a new key and body 20/1 persisted
// before the send; a cut (lost) response resumes that same new key after restart and
// repeated SyncAccess mints no second key. The old key is never sent with the 20/1 body.
func TestIntentFenceCRotationUsesCurrentMarkerRevisionsAndResumesOneKey(t *testing.T) {
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	f := newIntentFenceFixture(t, &subject)
	originalKey := f.firstSync(t, "op-1")
	const marker = `{"required":true,"catalog_revision":"20","binding_revision":"1"}`

	receipt := f.pollMarker(t, "op-1", "retryable_failure", marker)
	if !receipt.RecoveryRequired || receipt.RecoveryRevisions == nil ||
		receipt.RecoveryRevisions.CatalogRevision != "20" ||
		receipt.RecoveryRevisions.BindingRevision != "1" {
		t.Fatalf("valid marker must persist the pair: %+v", receipt)
	}
	if f.store.Value == nil || !f.store.Value.RecoveryRequired ||
		f.store.Value.RecoveryRevisions == nil ||
		f.store.Value.RecoveryRevisions.CatalogRevision != "20" {
		t.Fatalf("marker must be durable: %+v", f.store.Value)
	}

	f.observer.cutNextAttempt()
	if _, err := f.coordinator.SyncAccess(context.Background()); err == nil {
		t.Fatal("cut response must surface as a transport error")
	}
	attempts := f.observer.snapshot()
	if len(attempts) != 2 {
		t.Fatalf("POST count=%d want 2: %+v", len(attempts), attempts)
	}
	rotated := attempts[1]
	if rotated.key == originalKey {
		t.Fatalf("required marker must mint a new key: %s", rotated.key)
	}
	if !strings.Contains(rotated.body, `"catalog_revision":"20"`) ||
		!strings.Contains(rotated.body, `"binding_revision":"1"`) {
		t.Fatalf("rotated body must carry the marker revisions: %s", rotated.body)
	}
	if rotated.receipt == nil || rotated.receipt.Key != rotated.key ||
		rotated.receipt.Response != nil || rotated.receipt.RotationOf != "op-1" ||
		rotated.receipt.RecoveryRequired || rotated.receipt.RecoveryRevisions != nil {
		t.Fatalf("new key/body must be durably persisted before the send: %+v", rotated.receipt)
	}
	for _, attempt := range attempts {
		if attempt.key == originalKey && strings.Contains(attempt.body, `"catalog_revision":"20"`) {
			t.Fatalf("old key must never carry the 20/1 body (self-inflicted 409): %+v", attempt)
		}
	}
	if f.store.Value == nil || f.store.Value.Key != rotated.key ||
		f.store.Value.Response != nil || f.store.Value.RotationOf != "op-1" {
		t.Fatalf("rotated receipt wrong after lost response: %+v", f.store.Value)
	}

	// Restart (new Coordinator, same store/client) resumes exactly the persisted new key.
	restarted := f.restart(t)
	f.fake.syncResponse = syncBody("op-2", "retryable_failure")
	result, err := restarted.SyncAccess(context.Background())
	if err != nil || result.Receipt == nil {
		t.Fatalf("restart resume of the rotated intent: %+v %v", result, err)
	}
	attempts = f.observer.snapshot()
	if len(attempts) != 3 || attempts[2].key != rotated.key || attempts[2].body != rotated.body {
		t.Fatalf("restart must resume the persisted new key/body: %+v", attempts)
	}
	if attempts[2].receipt == nil || attempts[2].receipt.Key != rotated.key ||
		attempts[2].receipt.Response != nil {
		t.Fatalf("restart must not mint another key: %+v", attempts[2].receipt)
	}
	if f.store.Value == nil || f.store.Value.RotationOf != "op-1" ||
		f.store.Value.RecoveryRequired || f.store.Value.RecoveryRevisions != nil {
		t.Fatalf("resumed receipt must keep provenance and cleared marker: %+v", f.store.Value)
	}

	// Repeated SyncAccess on the retryable but unmarked new operation replays the same
	// key: the mandatory probe now reads op-2's own (unmarked) marker.
	f.fake.syncResponse = syncBody("op-2", "retryable_failure")
	f.fake.operationBody = fenceOperationBody("op-2", "retryable_failure", "", "")
	repeated, err := restarted.SyncAccess(context.Background())
	if err != nil || repeated.Receipt == nil {
		t.Fatalf("repeated sync: %+v %v", repeated, err)
	}
	attempts = f.observer.snapshot()
	if len(attempts) != 4 || attempts[3].key != rotated.key {
		t.Fatalf("repeated sync must not mint another key: %+v", attempts)
	}
	keys := distinctKeys(attempts)
	if len(keys) != 2 || !keys[originalKey] || !keys[rotated.key] {
		t.Fatalf("exactly two keys expected (old dead + one new): %v", keys)
	}
	assertKeyBodyPairing(t, attempts)
}

// TestIntentFenceDSameRevisionMarkerMintsFreshKeyWithSameBody is the (d) case: the server
// accepts a fresh key with exactly the marker revisions even when they equal the saved
// ones, so a 19/1 marker rotates to one new key with the identical 19/1 body.
func TestIntentFenceDSameRevisionMarkerMintsFreshKeyWithSameBody(t *testing.T) {
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	f := newIntentFenceFixture(t, &subject)
	originalKey := f.firstSync(t, "op-1")
	const marker = `{"required":true,"catalog_revision":"19","binding_revision":"1"}`

	receipt := f.pollMarker(t, "op-1", "retryable_failure", marker)
	if !receipt.RecoveryRequired || receipt.RecoveryRevisions == nil ||
		receipt.RecoveryRevisions.CatalogRevision != "19" {
		t.Fatalf("same-revision marker must persist the pair: %+v", receipt)
	}
	f.fake.syncResponse = syncBody("op-2", "retryable_failure")
	result, err := f.coordinator.SyncAccess(context.Background())
	if err != nil || result.Receipt == nil {
		t.Fatalf("same-revision rotation: %+v %v", result, err)
	}
	attempts := f.observer.snapshot()
	if len(attempts) != 2 || attempts[1].key == originalKey {
		t.Fatalf("exactly one new key expected: %+v", attempts)
	}
	if attempts[1].body != attempts[0].body {
		t.Fatalf("same-revision rotation must carry the identical 19/1 body:\n%s\n%s",
			attempts[1].body, attempts[0].body)
	}
	if attempts[1].receipt == nil || attempts[1].receipt.Key != attempts[1].key ||
		attempts[1].receipt.Response != nil || attempts[1].receipt.RotationOf != "op-1" {
		t.Fatalf("new key/body must be persisted before the send: %+v", attempts[1].receipt)
	}
	assertKeyBodyPairing(t, attempts)
}

// TestIntentFenceEInvalidMarkerRevisionsNeverPost is the (e) case: required=true with a
// missing or non-decimal pair returns a bounded error with NO POST at all and leaves the
// durable receipt exactly as-is (never a stale-body fallback).
func TestIntentFenceEInvalidMarkerRevisionsNeverPost(t *testing.T) {
	cases := []struct {
		name   string
		marker string
	}{
		{"missing pair", `{"required":true}`},
		{"non-decimal catalog", `{"required":true,"catalog_revision":"20x","binding_revision":"1"}`},
		{"non-decimal binding", `{"required":true,"catalog_revision":"19","binding_revision":"1x"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
			f := newIntentFenceFixture(t, &subject)
			originalKey := f.firstSync(t, "op-1")
			receipt := f.pollMarker(t, "op-1", "retryable_failure", testCase.marker)
			if !receipt.RecoveryRequired || receipt.RecoveryRevisions != nil {
				t.Fatalf("required marker without a valid pair must persist no pair: %+v", receipt)
			}
			attemptsBefore := len(f.observer.snapshot())

			if _, err := f.coordinator.SyncAccess(context.Background()); !errors.Is(err, ErrRecoveryRevisions) {
				t.Fatalf("invalid marker pair must return ErrRecoveryRevisions, got %v", err)
			}
			if got := len(f.observer.snapshot()); got != attemptsBefore {
				t.Fatalf("invalid marker pair must not POST: %d -> %d", attemptsBefore, got)
			}
			stored := f.store.Value
			if stored == nil || stored.Key != originalKey || stored.Response == nil ||
				stored.Response.AccessApplicationState != "retryable_failure" ||
				!stored.RecoveryRequired || stored.RecoveryRevisions != nil {
				t.Fatalf("durable receipt must stay exactly as-is: %+v", stored)
			}
		})
	}
}

// TestIntentFenceFRequiredFalseKeepsGenericResend is the (f) case: required=false with
// revisions present clears the hint and keeps the generic same-key/same-body resend.
func TestIntentFenceFRequiredFalseKeepsGenericResend(t *testing.T) {
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	f := newIntentFenceFixture(t, &subject)
	originalKey := f.firstSync(t, "op-1")
	const marker = `{"required":false,"catalog_revision":"20","binding_revision":"1"}`

	receipt := f.pollMarker(t, "op-1", "retryable_failure", marker)
	if receipt.RecoveryRequired || receipt.RecoveryRevisions != nil {
		t.Fatalf("required=false must persist no marker: %+v", receipt)
	}
	result, err := f.coordinator.SyncAccess(context.Background())
	if err != nil || result.Receipt == nil {
		t.Fatalf("required=false retry: %+v %v", result, err)
	}
	attempts := f.observer.snapshot()
	if len(attempts) != 2 || attempts[1].key != originalKey || attempts[1].body != attempts[0].body {
		t.Fatalf("required=false must resend the persisted key/body: %+v", attempts)
	}
	assertKeyBodyPairing(t, attempts)
}

// TestIntentFenceGConcurrencyMintsOneKeyAndMarkerScope guards the (g) cases: concurrent
// SyncAccess on a marked intent cannot mint two keys (existing syncMu), and a marker on a
// non-retryable aggregate is ignored.
func TestIntentFenceGConcurrencyMintsOneKeyAndMarkerScope(t *testing.T) {
	t.Run("concurrent rotation mints one key", func(t *testing.T) {
		subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
		f := newIntentFenceFixture(t, &subject)
		originalKey := f.firstSync(t, "op-1")
		receipt := f.pollMarker(t, "op-1", "retryable_failure",
			`{"required":true,"catalog_revision":"20","binding_revision":"1"}`)
		if !receipt.RecoveryRequired {
			t.Fatalf("marker must be durable: %+v", receipt)
		}
		f.fake.syncResponse = syncBody("op-2", "retryable_failure")
		// Trailing callers probe op-2's own marker; it is unmarked, so they resume the
		// single rotated key instead of rotating again.
		f.fake.operationBody = fenceOperationBody("op-2", "retryable_failure", "", "")
		f.observer.setSyncDelay(30 * time.Millisecond)

		const callers = 4
		var wait sync.WaitGroup
		results := make([]SyncResult, callers)
		errs := make([]error, callers)
		for index := 0; index < callers; index++ {
			wait.Add(1)
			go func(slot int) {
				defer wait.Done()
				results[slot], errs[slot] = f.coordinator.SyncAccess(context.Background())
			}(index)
		}
		wait.Wait()
		for index, err := range errs {
			if err != nil {
				t.Fatalf("caller %d: %v", index, err)
			}
		}
		attempts := f.observer.snapshot()
		if len(attempts) != callers+1 {
			t.Fatalf("POST count=%d want %d", len(attempts), callers+1)
		}
		keys := distinctKeys(attempts)
		if len(keys) != 2 || !keys[originalKey] {
			t.Fatalf("exactly one rotated key expected on top of the old one: %v", keys)
		}
		rotatedKey := ""
		for key := range keys {
			if key != originalKey {
				rotatedKey = key
			}
		}
		if f.store.Value == nil || f.store.Value.Key != rotatedKey {
			t.Fatalf("durable receipt must carry the single rotated key: %+v", f.store.Value)
		}
		for _, attempt := range attempts[1:] {
			if attempt.key != rotatedKey {
				t.Fatalf("trailing callers must resume the rotated key, got %q want %q", attempt.key, rotatedKey)
			}
		}
		assertKeyBodyPairing(t, attempts)
	})

	t.Run("marker on a non-retryable aggregate is ignored", func(t *testing.T) {
		subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
		f := newIntentFenceFixture(t, &subject)
		f.refreshMe(t, "7", strPtr("1"), "active")
		f.refreshCatalog(t, "19")
		f.fake.syncResponse = syncBody("op-1", "pending")
		if _, err := f.coordinator.SyncAccess(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.fake.operationBody = fenceOperationBody("op-1", "applied", "",
			`{"required":true,"catalog_revision":"20","binding_revision":"1"}`)
		poll, err := f.coordinator.PollAccessOperation(context.Background())
		if err != nil || !poll.Terminal {
			t.Fatalf("applied poll: %+v %v", poll, err)
		}
		if f.store.Value == nil || f.store.Value.RecoveryRequired ||
			f.store.Value.RecoveryRevisions != nil {
			t.Fatalf("marker on an applied aggregate must be ignored: %+v", f.store.Value)
		}
		attemptsBefore := len(f.observer.snapshot())
		replay, err := f.coordinator.SyncAccess(context.Background())
		if err != nil || !replay.Replayed || replay.Receipt == nil {
			t.Fatalf("terminal replay: %+v %v", replay, err)
		}
		if got := len(f.observer.snapshot()); got != attemptsBefore {
			t.Fatalf("terminal replay must not POST: %d -> %d", attemptsBefore, got)
		}
	})
}

// TestIntentFenceHRetiredFieldsStrictRejected is the (h) case and the retired-branch
// proof: the old new_intent_required/recovery/revisions fields and the old per-node
// recoverable_now predicate are unknown to this contract and never rotate.
func TestIntentFenceHRetiredFieldsStrictRejected(t *testing.T) {
	base := fenceOperationBody("op-1", "retryable_failure", "", "")
	retired := map[string]string{
		"new_intent_required":  `"new_intent_required":true`,
		"old recovery object":  `"recovery":{"recoverable_now":true,"revisions":{"catalog_revision":"20","binding_revision":"1"}}`,
		"old revisions object": `"revisions":{"catalog_revision":"20","binding_revision":"1"}`,
	}
	for name, field := range retired {
		body := strings.Replace(base, `"per_node":[]`, `"per_node":[],`+field, 1)
		if err := decodeStrict([]byte(body), &OperationResponse{}); err == nil {
			t.Fatalf("%s must be rejected as an unknown field", name)
		}
	}

	// A per-node recoverable_now state is not this contract: it decodes as an opaque
	// string and never marks the receipt.
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	f := newIntentFenceFixture(t, &subject)
	originalKey := f.firstSync(t, "op-1")
	f.fake.operationBody = fenceOperationBody("op-1", "retryable_failure",
		`[{"gateway_id":"gw-1","state":"recoverable_now","attempts":12,"applied_generation":null,"not_after":null,"last_error":"admin_socket"}]`, "")
	poll, err := f.coordinator.PollAccessOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if poll.Receipt == nil || poll.Receipt.RecoveryRequired || poll.Receipt.RecoveryRevisions != nil {
		t.Fatalf("per-node recoverable_now must not mark the receipt: %+v", poll.Receipt)
	}
	f.refreshCatalog(t, "20")
	if _, err := f.coordinator.SyncAccess(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempts := f.observer.snapshot()
	if len(attempts) != 2 || attempts[1].key != originalKey {
		t.Fatalf("retired per-node predicate must not rotate: %+v", attempts)
	}
	assertKeyBodyPairing(t, attempts)
}

// TestIntentFenceReceiptRecoveryFieldsRoundTripAndLegacyBlobAbsent pins the durable
// receipt shape: old blobs decode with zero marker fields, and all three new fields
// survive a real namespace/version round trip.
func TestIntentFenceReceiptRecoveryFieldsRoundTripAndLegacyBlobAbsent(t *testing.T) {
	var persisted []byte
	persist := func(_ context.Context, payload []byte) error {
		persisted = append([]byte(nil), payload...)
		return nil
	}
	legacy := []byte(`{"namespace":"accountaccess_receipt_v1","version":1,"receipt":{` +
		`"subject":{"account_ref":"acc-1","installation_id":"inst-1"},` +
		`"key":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","body_digest":"digest",` +
		`"request":{"catalog_revision":"19","binding_revision":"1"}}}`)
	store, err := NewBridgeReceiptStore(legacy, persist)
	if err != nil {
		t.Fatalf("legacy receipt namespace/version must stay decodable: %v", err)
	}
	loaded, err := store.Load()
	if err != nil || loaded == nil || loaded.RecoveryRequired ||
		loaded.RecoveryRevisions != nil || loaded.RotationOf != "" {
		t.Fatalf("legacy receipt must decode with zero marker fields: %+v %v", loaded, err)
	}
	loaded.RecoveryRequired = true
	loaded.RecoveryRevisions = &AccessSyncRequest{CatalogRevision: "20", BindingRevision: "1"}
	loaded.RotationOf = "op-dead-1"
	if err := store.Save(*loaded); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewBridgeReceiptStore(persisted, persist)
	if err != nil {
		t.Fatal(err)
	}
	roundTripped, err := reloaded.Load()
	if err != nil || roundTripped == nil || !roundTripped.RecoveryRequired ||
		roundTripped.RecoveryRevisions == nil ||
		roundTripped.RecoveryRevisions.CatalogRevision != "20" ||
		roundTripped.RecoveryRevisions.BindingRevision != "1" || roundTripped.RotationOf != "op-dead-1" {
		t.Fatalf("marker fields must survive a durable round trip: %+v %v", roundTripped, err)
	}
}

// TestIntentFenceIProbeBeforeGenericResendRotatesOnceAndResumes is the (a) case: a durable
// 19/1 retryable_failure receipt without a prior poll now probes its own operation from
// inside SyncAccess (exactly one opt-in GET), rotates on the observed required marker, and
// persists the new key/20/1 body before the POST. The lost rotated response resumes the
// same key without any further probe, and the new operation's own unmarked marker never
// rotates again.
func TestIntentFenceIProbeBeforeGenericResendRotatesOnceAndResumes(t *testing.T) {
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	f := newIntentFenceFixture(t, &subject)
	originalKey := f.firstSync(t, "op-1")
	const marker = `{"required":true,"catalog_revision":"20","binding_revision":"1"}`

	if reads := f.observer.readSnapshot(); len(reads) != 0 {
		t.Fatalf("the fresh first sync must not probe: %v", reads)
	}
	f.fake.operationBody = fenceOperationBody("op-1", "retryable_failure", "", marker)

	f.observer.cutNextAttempt()
	if _, err := f.coordinator.SyncAccess(context.Background()); err == nil {
		t.Fatal("cut rotated response must surface as a transport error")
	}
	attempts := f.observer.snapshot()
	if len(attempts) != 2 {
		t.Fatalf("POST count=%d want 2: %+v", len(attempts), attempts)
	}
	reads := f.observer.readSnapshot()
	if len(reads) != 1 || reads[0] != "GET /operations/op-1?observe=sync_recovery" {
		t.Fatalf("exactly one opt-in probe expected before the resend: %v", reads)
	}
	rotated := attempts[1]
	if rotated.key == originalKey {
		t.Fatalf("observed required marker must mint a new key: %s", rotated.key)
	}
	if !strings.Contains(rotated.body, `"catalog_revision":"20"`) ||
		!strings.Contains(rotated.body, `"binding_revision":"1"`) {
		t.Fatalf("rotated body must carry the marker revisions: %s", rotated.body)
	}
	if rotated.receipt == nil || rotated.receipt.Key != rotated.key ||
		rotated.receipt.Response != nil || rotated.receipt.RotationOf != "op-1" ||
		rotated.receipt.RecoveryRequired || rotated.receipt.RecoveryRevisions != nil {
		t.Fatalf("new key/body must be durably persisted before the send: %+v", rotated.receipt)
	}

	// A restart resume of the lost rotated response is never probed: Response == nil has
	// an unknown outcome and no operation id to poll.
	restarted := f.restart(t)
	f.fake.syncResponse = syncBody("op-2", "retryable_failure")
	result, err := restarted.SyncAccess(context.Background())
	if err != nil || result.Receipt == nil {
		t.Fatalf("restart resume of the rotated intent: %+v %v", result, err)
	}
	attempts = f.observer.snapshot()
	if len(attempts) != 3 || attempts[2].key != rotated.key ||
		attempts[2].body != rotated.body || attempts[2].receipt.Response != nil {
		t.Fatalf("restart must resume the persisted new key/body without a probe: %+v", attempts)
	}
	if reads = f.observer.readSnapshot(); len(reads) != 1 {
		t.Fatalf("lost-response resume must not probe: %v", reads)
	}

	// op-2's own marker is unmarked: a later probe still happens but mints no second key.
	f.fake.operationBody = fenceOperationBody("op-2", "retryable_failure", "", "")
	repeated, err := restarted.SyncAccess(context.Background())
	if err != nil || repeated.Receipt == nil {
		t.Fatalf("repeated sync: %+v %v", repeated, err)
	}
	attempts = f.observer.snapshot()
	if len(attempts) != 4 || attempts[3].key != rotated.key || attempts[3].body != rotated.body {
		t.Fatalf("later unmarked probe must not rotate again: %+v", attempts)
	}
	reads = f.observer.readSnapshot()
	if len(reads) != 2 || reads[1] != "GET /operations/op-2?observe=sync_recovery" {
		t.Fatalf("second probe must target the current operation: %v", reads)
	}
	keys := distinctKeys(attempts)
	if len(keys) != 2 || !keys[originalKey] || !keys[rotated.key] {
		t.Fatalf("exactly two keys expected (old dead + one rotated): %v", keys)
	}
	assertKeyBodyPairing(t, attempts)
}

// TestIntentFenceJProbeRequiredFalseResendsAndReprobes is the (b) case: required=false
// probes exactly once per SyncAccess invocation and resends the SAME key/body; there is no
// "already probed" cache, so the next invocation probes again.
func TestIntentFenceJProbeRequiredFalseResendsAndReprobes(t *testing.T) {
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	f := newIntentFenceFixture(t, &subject)
	originalKey := f.firstSync(t, "op-1")
	f.fake.operationBody = fenceOperationBody("op-1", "retryable_failure", "",
		`{"required":false,"catalog_revision":"20","binding_revision":"1"}`)
	f.fake.syncResponse = syncBody("op-1", "retryable_failure")

	for round := 1; round <= 2; round++ {
		result, err := f.coordinator.SyncAccess(context.Background())
		if err != nil || result.Receipt == nil {
			t.Fatalf("round %d: required=false resend: %+v %v", round, result, err)
		}
		reads := f.observer.readSnapshot()
		if len(reads) != round || reads[round-1] != "GET /operations/op-1?observe=sync_recovery" {
			t.Fatalf("round %d must issue exactly one probe: %v", round, reads)
		}
	}
	attempts := f.observer.snapshot()
	if len(attempts) != 3 {
		t.Fatalf("POST count=%d want 3: %+v", len(attempts), attempts)
	}
	for _, attempt := range attempts[1:] {
		if attempt.key != originalKey || attempt.body != attempts[0].body {
			t.Fatalf("required=false must resend the persisted key/body: %+v", attempts)
		}
	}
	if f.store.Value == nil || f.store.Value.RecoveryRequired || f.store.Value.RecoveryRevisions != nil {
		t.Fatalf("required=false must persist no marker: %+v", f.store.Value)
	}
	assertKeyBodyPairing(t, attempts)
}

// TestIntentFenceKProbeFailureFallsBackToPersistedResend is the (c) case: a failing probe
// (HTTP error) never rotates and never surfaces a new terminal error; the same persisted
// key/body is resent exactly as before.
func TestIntentFenceKProbeFailureFallsBackToPersistedResend(t *testing.T) {
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	f := newIntentFenceFixture(t, &subject)
	originalKey := f.firstSync(t, "op-1")
	f.fake.operationStatus = http.StatusServiceUnavailable
	f.fake.operationBody = ""
	f.fake.syncResponse = syncBody("op-1", "retryable_failure")

	result, err := f.coordinator.SyncAccess(context.Background())
	if err != nil || result.Receipt == nil {
		t.Fatalf("probe failure must not surface a new terminal error: %+v %v", result, err)
	}
	attempts := f.observer.snapshot()
	if len(attempts) != 2 || attempts[1].key != originalKey || attempts[1].body != attempts[0].body {
		t.Fatalf("probe failure must resend the persisted key/body: %+v", attempts)
	}
	reads := f.observer.readSnapshot()
	if len(reads) != 1 || reads[0] != "GET /operations/op-1?observe=sync_recovery" {
		t.Fatalf("exactly one bounded probe attempt expected: %v", reads)
	}
	if f.store.Value == nil || f.store.Value.RecoveryRequired || f.store.Value.RecoveryRevisions != nil {
		t.Fatalf("a failed probe must not mark recovery: %+v", f.store.Value)
	}
	assertKeyBodyPairing(t, attempts)
}

// TestIntentFenceLLostResponseSkipsProbe is the (d) case: a persisted receipt with
// Response == nil has an unknown outcome and no operation id; it resends the same key/body
// WITHOUT any opt-in GET, even when a marker would otherwise be available.
func TestIntentFenceLLostResponseSkipsProbe(t *testing.T) {
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	f := newIntentFenceFixture(t, &subject)
	f.refreshMe(t, "7", strPtr("1"), "active")
	f.refreshCatalog(t, "19")
	f.fake.syncResponse = syncBody("op-1", "retryable_failure")
	f.observer.cutNextAttempt()
	if _, err := f.coordinator.SyncAccess(context.Background()); err == nil {
		t.Fatal("cut response must surface as a transport error")
	}
	attempts := f.observer.snapshot()
	if len(attempts) != 1 || f.store.Value == nil || f.store.Value.Response != nil {
		t.Fatalf("lost response must persist an uncertain receipt: %+v", f.store.Value)
	}
	originalKey := attempts[0].key
	f.fake.operationBody = fenceOperationBody("op-1", "retryable_failure", "",
		`{"required":true,"catalog_revision":"20","binding_revision":"1"}`)

	result, err := f.coordinator.SyncAccess(context.Background())
	if err != nil || result.Receipt == nil {
		t.Fatalf("lost-response resume: %+v %v", result, err)
	}
	attempts = f.observer.snapshot()
	if len(attempts) != 2 || attempts[1].key != originalKey || attempts[1].body != attempts[0].body {
		t.Fatalf("lost response must resend the same key/body: %+v", attempts)
	}
	if reads := f.observer.readSnapshot(); len(reads) != 0 {
		t.Fatalf("a lost response must not probe: %v", reads)
	}
	if f.store.Value == nil || f.store.Value.RecoveryRequired || f.store.Value.RecoveryRevisions != nil {
		t.Fatalf("a lost response must not consume a marker: %+v", f.store.Value)
	}
	assertKeyBodyPairing(t, attempts)
}

// TestIntentFenceMFreshIntentPostsWithoutProbe is the (e) case: with no receipt there is no
// old operation to probe, so a fresh intent POSTs without any GET even when the server
// would answer a marker.
func TestIntentFenceMFreshIntentPostsWithoutProbe(t *testing.T) {
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	f := newIntentFenceFixture(t, &subject)
	f.refreshMe(t, "7", strPtr("1"), "active")
	f.refreshCatalog(t, "19")
	f.fake.operationBody = fenceOperationBody("op-1", "retryable_failure", "",
		`{"required":true,"catalog_revision":"20","binding_revision":"1"}`)
	f.fake.syncResponse = syncBody("op-1", "applied")

	result, err := f.coordinator.SyncAccess(context.Background())
	if err != nil || result.Receipt == nil || result.Receipt.Response == nil ||
		result.Receipt.Response.AccessApplicationState != "applied" {
		t.Fatalf("fresh intent sync: %+v %v", result, err)
	}
	if attempts := f.observer.snapshot(); len(attempts) != 1 {
		t.Fatalf("fresh intent must POST exactly once: %+v", attempts)
	}
	if reads := f.observer.readSnapshot(); len(reads) != 0 {
		t.Fatalf("fresh intent must not probe: %v", reads)
	}
}

// TestIntentFenceNProbeRequiredMissingRevisionsNeverPosts is the (f) case: the probe
// observes required=true without a usable pair, so SyncAccess returns ErrRecoveryRevisions
// with NO POST (not even the stale body) and no further probe on the next invocation; an
// explicit later poll that supplies a valid pair re-enables exactly one rotation.
func TestIntentFenceNProbeRequiredMissingRevisionsNeverPosts(t *testing.T) {
	cases := []struct {
		name   string
		marker string
	}{
		{"missing pair", `{"required":true}`},
		{"non-decimal catalog", `{"required":true,"catalog_revision":"20x","binding_revision":"1"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
			f := newIntentFenceFixture(t, &subject)
			originalKey := f.firstSync(t, "op-1")
			f.fake.operationBody = fenceOperationBody("op-1", "retryable_failure", "", testCase.marker)

			if _, err := f.coordinator.SyncAccess(context.Background()); !errors.Is(err, ErrRecoveryRevisions) {
				t.Fatalf("probed invalid marker pair must return ErrRecoveryRevisions, got %v", err)
			}
			attempts := f.observer.snapshot()
			if len(attempts) != 1 {
				t.Fatalf("invalid marker pair must not POST: %+v", attempts)
			}
			reads := f.observer.readSnapshot()
			if len(reads) != 1 || reads[0] != "GET /operations/op-1?observe=sync_recovery" {
				t.Fatalf("exactly one probe expected: %v", reads)
			}
			stored := f.store.Value
			if stored == nil || stored.Key != originalKey || stored.Response == nil ||
				stored.Response.AccessApplicationState != "retryable_failure" ||
				!stored.RecoveryRequired || stored.RecoveryRevisions != nil {
				t.Fatalf("durable receipt must stay exactly as probed: %+v", stored)
			}

			// The durable required-without-pair hint keeps blocking without another probe.
			if _, err := f.coordinator.SyncAccess(context.Background()); !errors.Is(err, ErrRecoveryRevisions) {
				t.Fatalf("durable invalid hint must stay bounded, got %v", err)
			}
			if got := len(f.observer.snapshot()); got != 1 {
				t.Fatalf("durable invalid hint must not POST: %d", got)
			}
			if got := len(f.observer.readSnapshot()); got != 1 {
				t.Fatalf("durable invalid hint must not re-probe: %d", got)
			}

			// A later explicit poll supplies a usable pair: exactly one rotation follows.
			f.pollMarker(t, "op-1", "retryable_failure",
				`{"required":true,"catalog_revision":"20","binding_revision":"1"}`)
			f.fake.syncResponse = syncBody("op-2", "retryable_failure")
			result, err := f.coordinator.SyncAccess(context.Background())
			if err != nil || result.Receipt == nil {
				t.Fatalf("rotation after a refreshed marker: %+v %v", result, err)
			}
			attempts = f.observer.snapshot()
			if len(attempts) != 2 || attempts[1].key == originalKey ||
				!strings.Contains(attempts[1].body, `"catalog_revision":"20"`) {
				t.Fatalf("refreshed marker must rotate exactly once: %+v", attempts)
			}
			assertKeyBodyPairing(t, attempts)
		})
	}
}

// TestIntentFenceOPendingAndConcurrencyProbeCadence is the (g) case: the pending path is
// unchanged (no probe, no POST), and concurrent generic resends still single-flight through
// syncMu: every caller probes once, every caller resends the one persisted key, and no
// extra key is minted.
func TestIntentFenceOPendingAndConcurrencyProbeCadence(t *testing.T) {
	t.Run("pending replays without probe or post", func(t *testing.T) {
		subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
		f := newIntentFenceFixture(t, &subject)
		f.refreshMe(t, "7", strPtr("1"), "active")
		f.refreshCatalog(t, "19")
		f.fake.syncResponse = syncBody("op-1", "pending")
		if _, err := f.coordinator.SyncAccess(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.fake.operationBody = fenceOperationBody("op-1", "pending", "",
			`{"required":true,"catalog_revision":"20","binding_revision":"1"}`)

		replay, err := f.coordinator.SyncAccess(context.Background())
		if err != nil || !replay.Replayed {
			t.Fatalf("pending replay: %+v %v", replay, err)
		}
		if attempts := f.observer.snapshot(); len(attempts) != 1 {
			t.Fatalf("pending replay must not POST: %+v", attempts)
		}
		if reads := f.observer.readSnapshot(); len(reads) != 0 {
			t.Fatalf("pending replay must not probe: %v", reads)
		}
	})

	t.Run("concurrent generic resends probe once each and mint no key", func(t *testing.T) {
		subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
		f := newIntentFenceFixture(t, &subject)
		originalKey := f.firstSync(t, "op-1")
		f.fake.operationBody = fenceOperationBody("op-1", "retryable_failure", "", "")
		f.fake.syncResponse = syncBody("op-1", "retryable_failure")

		const callers = 4
		var wait sync.WaitGroup
		errs := make([]error, callers)
		for index := 0; index < callers; index++ {
			wait.Add(1)
			go func(slot int) {
				defer wait.Done()
				_, errs[slot] = f.coordinator.SyncAccess(context.Background())
			}(index)
		}
		wait.Wait()
		for index, err := range errs {
			if err != nil {
				t.Fatalf("caller %d: %v", index, err)
			}
		}
		attempts := f.observer.snapshot()
		if len(attempts) != callers+1 {
			t.Fatalf("POST count=%d want %d: %+v", len(attempts), callers+1, attempts)
		}
		keys := distinctKeys(attempts)
		if len(keys) != 1 || !keys[originalKey] {
			t.Fatalf("concurrent generic resends must mint no key: %v", keys)
		}
		if reads := f.observer.readSnapshot(); len(reads) != callers {
			t.Fatalf("each invocation must probe exactly once: %d want %d", len(reads), callers)
		}
		assertKeyBodyPairing(t, attempts)
	})
}

// TestIntentFencePProbePendingTransitionNeverPosts is the post-poll state case: the
// read-only probe advances the durable operation to pending, so the generic resend must
// not POST for an operation that polling now owns.
func TestIntentFencePProbePendingTransitionNeverPosts(t *testing.T) {
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	f := newIntentFenceFixture(t, &subject)
	originalKey := f.firstSync(t, "op-1")
	f.fake.operationBody = fenceOperationBody("op-1", "pending", "", "")

	result, err := f.coordinator.SyncAccess(context.Background())
	if err != nil || !result.Replayed || result.Receipt == nil {
		t.Fatalf("pending transition must replay without a POST: %+v %v", result, err)
	}
	if got := len(f.observer.snapshot()); got != 1 {
		t.Fatalf("pending transition must not POST: %d -> %d", 1, got)
	}
	if result.Receipt.Key != originalKey ||
		result.Receipt.Response == nil ||
		result.Receipt.Response.AccessApplicationState != "pending" {
		t.Fatalf("durable receipt must carry the probed pending state: %+v", result.Receipt)
	}
	if f.store.Value == nil || f.store.Value.Response == nil ||
		f.store.Value.Response.AccessApplicationState != "pending" {
		t.Fatalf("pending state must be durable: %+v", f.store.Value)
	}
	reads := f.observer.readSnapshot()
	if len(reads) != 1 || reads[0] != "GET /operations/op-1?observe=sync_recovery" {
		t.Fatalf("exactly one opt-in probe expected: %v", reads)
	}
}

// TestIntentFenceQProbeTerminalTransitionFollowsTerminalSemantics is the post-poll
// terminal case: after the probe the operation is applied/rejected, so SyncAccess follows
// the existing terminal admissible-body semantics - the same digest replays without a
// POST and a changed admissible body starts one new keyed intent, never a stale POST.
func TestIntentFenceQProbeTerminalTransitionFollowsTerminalSemantics(t *testing.T) {
	t.Run("applied same digest replays", func(t *testing.T) {
		subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
		f := newIntentFenceFixture(t, &subject)
		originalKey := f.firstSync(t, "op-1")
		f.fake.operationBody = fenceOperationBody("op-1", "applied", "", "")

		replay, err := f.coordinator.SyncAccess(context.Background())
		if err != nil || !replay.Replayed || replay.Receipt == nil {
			t.Fatalf("terminal probe with the same digest must replay: %+v %v", replay, err)
		}
		if replay.Receipt.Key != originalKey {
			t.Fatalf("terminal replay must keep the persisted key: %s", replay.Receipt.Key)
		}
		if got := len(f.observer.snapshot()); got != 1 {
			t.Fatalf("terminal replay must not POST: %d -> %d", 1, got)
		}
		if reads := f.observer.readSnapshot(); len(reads) != 1 {
			t.Fatalf("exactly one opt-in probe expected: %v", reads)
		}
	})

	t.Run("rejected changed digest starts one new keyed intent", func(t *testing.T) {
		subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
		f := newIntentFenceFixture(t, &subject)
		originalKey := f.firstSync(t, "op-1")
		f.refreshCatalog(t, "20")
		f.fake.operationBody = fenceOperationBody("op-1", "rejected", "", "")
		f.fake.syncResponse = syncBody("op-2", "retryable_failure")

		result, err := f.coordinator.SyncAccess(context.Background())
		if err != nil || result.Receipt == nil {
			t.Fatalf("terminal probe with a changed digest: %+v %v", result, err)
		}
		attempts := f.observer.snapshot()
		if len(attempts) != 2 {
			t.Fatalf("changed terminal digest must POST exactly once: %+v", attempts)
		}
		rotated := attempts[1]
		if rotated.key == originalKey {
			t.Fatalf("changed terminal digest must mint a new key: %s", rotated.key)
		}
		if !strings.Contains(rotated.body, `"catalog_revision":"20"`) {
			t.Fatalf("new intent body must carry the fresh revision: %s", rotated.body)
		}
		if rotated.receipt == nil || rotated.receipt.Key != rotated.key {
			t.Fatalf("new key must be persisted before the send: %+v", rotated.receipt)
		}
		if reads := f.observer.readSnapshot(); len(reads) != 1 {
			t.Fatalf("exactly one opt-in probe expected: %v", reads)
		}
		assertKeyBodyPairing(t, attempts)
	})
}

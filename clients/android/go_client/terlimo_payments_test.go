package main

// S5 payment bridge tests. They run the real managed bridge read loop and the real
// managedMobile.run dispatch against the controlled local account-access fixture, with
// the contract payment fixtures pinned by sha256. No live endpoint, no install, no
// entitlement side effect.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"wg-turn-client/accountaccess"
	"wg-turn-client/servicechannel"
)

const (
	paymentBridgePlansFixtureSHA    = "fb71eca0fb7938bd898d0d8fdfacef037c8eae0a62a7e56f8d75b88b1dcef438"
	paymentBridgeQuoteFixtureSHA    = "eda35cea8097159be5b2f2145e38a8896713d2fb97b28a44ca4e8239a250e202"
	paymentBridgePaymentFixtureSHA  = "a61fdbf2cdc701a15cd8786dfdfbae7eb28dcedb9b43eeb288198b08790d6561"
	paymentBridgeCheckoutFixtureSHA = "31a11bbeb803e20edcb7e0b36d5fe0bd4ff916b6c9cebd889854c457ea5c862e"
	paymentBridgeErrorFixtureSHA    = "4fb377685516c8bc7255d8d7af9ec77dee2d49e9ff3706d2b0b010a057f25de1"
)

func paymentBridgeFixture(t *testing.T, name, wantSHA string) []byte {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("payment bridge fixture: runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "testdata", "step034-payments", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("payment bridge fixture %s: %v", name, err)
	}
	digest := sha256.Sum256(raw)
	if got := hex.EncodeToString(digest[:]); got != wantSHA {
		t.Fatalf("payment bridge fixture %s sha256 mismatch: got %s want %s; aborting", name, got, wantSHA)
	}
	return raw
}

// syncBuffer is the concurrency-safe bridge sink: the runner emitter and the payment
// action loop both write to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(raw []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(raw)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type capturedBridgeRequest struct {
	Method string
	Path   string
	Key    string
	Auth   string
	Body   string
}

type paymentHarness struct {
	t      *testing.T
	output *syncBuffer
	writer *io.PipeWriter
	cancel context.CancelFunc
	done   chan struct{}
	mobile *managedMobile

	mu       sync.Mutex
	requests []capturedBridgeRequest
}

// newPaymentHarness wires the full mobile lifecycle (real session, real runner, real
// bridge read loop) against the controlled local fixture. paymentResponder, when
// non-nil, is consulted first for every request and may serve the payment routes.
func newPaymentHarness(t *testing.T, paymentResponder func(w http.ResponseWriter, r *http.Request) bool) *paymentHarness {
	t.Helper()
	plansRaw := paymentBridgeFixture(t, "plans_response.json", paymentBridgePlansFixtureSHA)
	quoteRaw := paymentBridgeFixture(t, "quote_response.json", paymentBridgeQuoteFixtureSHA)
	paymentRaw := paymentBridgeFixture(t, "payment_response.json", paymentBridgePaymentFixtureSHA)
	checkoutRaw := paymentBridgeFixture(t, "checkout_session_response.json", paymentBridgeCheckoutFixtureSHA)

	fixture := newWiringFixture(t)
	harness := &paymentHarness{t: t, output: &syncBuffer{}, done: make(chan struct{})}
	writeBody := func(writer http.ResponseWriter, raw []byte) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(raw)
	}
	base := fixture.handler()
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		request.Body = io.NopCloser(bytes.NewReader(body))
		harness.mu.Lock()
		harness.requests = append(harness.requests, capturedBridgeRequest{
			Method: request.Method, Path: request.URL.Path,
			Key: request.Header.Get("Idempotency-Key"), Auth: request.Header.Get("Authorization"),
			Body: string(body),
		})
		harness.mu.Unlock()
		if paymentResponder != nil && paymentResponder(writer, request) {
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/mobile/v1/plans":
			writeBody(writer, plansRaw)
			return
		case request.Method == http.MethodPost && request.URL.Path == "/api/mobile/v1/quotes":
			writeBody(writer, quoteRaw)
			return
		case request.Method == http.MethodPost && request.URL.Path == "/api/mobile/v1/payments":
			writeBody(writer, paymentRaw)
			return
		case request.Method == http.MethodGet && request.URL.Path == "/api/mobile/v1/payments/pay-1":
			writeBody(writer, paymentRaw)
			return
		case request.Method == http.MethodPost && request.URL.Path == "/api/mobile/v1/payments/pay-1/checkout-session":
			writeBody(writer, checkoutRaw)
			return
		}
		base.ServeHTTP(writer, request)
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	harness.cancel = cancel
	bridge := newManagedBridge(harness.output, "attempt", cancel)
	pipeReader, pipeWriter := io.Pipe()
	harness.writer = pipeWriter
	go bridge.read(ctx, bufio.NewScanner(pipeReader))

	start := managedStart{V: 1, Type: "start", AttemptID: "attempt",
		MobileBaseURL: server.URL, MobileEnvironment: "test"}
	signer := func(ctx context.Context, transcript []byte) ([]byte, error) {
		digest := sha256.Sum256(transcript)
		return ecdsa.SignASN1(rand.Reader, fixture.key, digest[:])
	}
	controller := &managedController{bridge: bridge, start: managedStart{MobileBaseURL: server.URL}}
	mobile, err := newManagedMobile(start, fixture.spkiDER, signer, bridge, controller)
	if err != nil {
		cancel()
		t.Fatalf("newManagedMobile: %v", err)
	}
	harness.mobile = mobile
	// This harness pins the original v1 fixtures; managed production opts into v2.
	mobile.client.PaymentContract = 0
	go func() {
		mobile.run(ctx, bridge)
		close(harness.done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = pipeWriter.Close()
		select {
		case <-harness.done:
		case <-time.After(5 * time.Second):
		}
	})
	return harness
}

func (h *paymentHarness) waitForSubstring(marker string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for !strings.Contains(h.output.String(), marker) {
		if time.Now().After(deadline) {
			h.t.Fatalf("bridge output never contained %q: %s", marker, h.output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *paymentHarness) waitForEvent(event string, timeout time.Duration) bridgeMessage {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, line := range strings.Split(strings.TrimSpace(h.output.String()), "\n") {
			var message bridgeMessage
			if json.Unmarshal([]byte(line), &message) != nil {
				continue
			}
			if message.string("type") == event {
				return message
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("event %q never reached the bridge: %s", event, h.output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForEventCount blocks until at least want events of one type arrived and returns
// the last one.
func (h *paymentHarness) waitForEventCount(event string, want int, timeout time.Duration) bridgeMessage {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var last bridgeMessage
		count := 0
		for _, line := range strings.Split(strings.TrimSpace(h.output.String()), "\n") {
			var message bridgeMessage
			if json.Unmarshal([]byte(line), &message) != nil {
				continue
			}
			if message.string("type") == event {
				last = message
				count++
			}
		}
		if count >= want {
			return last
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("wanted %d %q events, saw %d: %s", want, event, count, h.output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *paymentHarness) sendAction(action bridgeMessage) {
	h.t.Helper()
	action["v"] = 1
	action["attempt_id"] = "attempt"
	raw, err := json.Marshal(action)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.writer.Write(append(raw, '\n')); err != nil {
		h.t.Fatalf("write host action: %v", err)
	}
}

func (h *paymentHarness) countPath(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, request := range h.requests {
		if request.Path == path {
			count++
		}
	}
	return count
}

func (h *paymentHarness) requestsFor(path string) []capturedBridgeRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []capturedBridgeRequest
	for _, request := range h.requests {
		if request.Path == path {
			out = append(out, request)
		}
	}
	return out
}

func (h *paymentHarness) eventCount(event string) int {
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(h.output.String()), "\n") {
		var message bridgeMessage
		if json.Unmarshal([]byte(line), &message) == nil && message.string("type") == event {
			count++
		}
	}
	return count
}

// TestPaymentBridgeActionRoundTrip pins the five frozen action/event pairs end to end:
// host line -> bridge read -> managedMobile.run dispatch -> contract client -> result
// event, with the host-owned Idempotency-Key captured verbatim on the wire.
func TestPaymentBridgeActionRoundTrip(t *testing.T) {
	harness := newPaymentHarness(t, nil)
	harness.waitForSubstring(`"type":"account_access"`, 5*time.Second)

	meBefore := harness.countPath("/api/mobile/v1/me")
	accessBefore := harness.eventCount("account_access")

	harness.sendAction(bridgeMessage{"type": paymentActionPlansList})
	plans := harness.waitForEvent(paymentEventPlansList, 5*time.Second)
	if plans.string("state") != "ok" || plans.string("attempt_id") != "attempt" ||
		plans.string("plans_revision") != "5" {
		t.Fatalf("plans_list_result wrong: %+v", plans)
	}
	planList, ok := plans["plans"].([]any)
	if !ok || len(planList) != 1 {
		t.Fatalf("plans_list_result plans wrong: %+v", plans)
	}
	plan, _ := planList[0].(map[string]any)
	if plan["plan_id"] != "p30" || plan["duration_code"] != "days:30" {
		t.Fatalf("plans_list_result plan wrong: %+v", plan)
	}
	amount, _ := plan["amount"].(map[string]any)
	if amount["amount_minor"] != float64(30000) || amount["currency"] != "RUB" {
		t.Fatalf("plans_list_result amount wrong: %+v", plan)
	}

	const quoteKey = "host-key-00000000000000000000000011"
	harness.sendAction(bridgeMessage{"type": paymentActionQuoteCreate, "plan_id": "p30",
		"duration_code": "days:30", "method": "card", "idempotency_key": quoteKey})
	quote := harness.waitForEvent(paymentEventQuoteCreate, 5*time.Second)
	if quote.string("state") != "ok" || quote.string("quote_id") != "q-1" ||
		quote.string("duration_code") != "days:30" || quote.string("expires_at") != "2026-09-19T15:35:00Z" {
		t.Fatalf("quote_create_result wrong: %+v", quote)
	}
	quoteRequests := harness.requestsFor("/api/mobile/v1/quotes")
	if len(quoteRequests) != 1 || quoteRequests[0].Key != quoteKey {
		t.Fatalf("quote request lost the host key: %+v", quoteRequests)
	}
	if quoteRequests[0].Body != `{"plan_id":"p30","duration_code":"days:30","method":"card"}` {
		t.Fatalf("quote request body wrong: %q", quoteRequests[0].Body)
	}

	const paymentKey = "host-key-00000000000000000000000012"
	harness.sendAction(bridgeMessage{"type": paymentActionPaymentCreate, "quote_id": "q-1",
		"idempotency_key": paymentKey})
	payment := harness.waitForEvent(paymentEventPaymentCreate, 5*time.Second)
	if payment.string("state") != "ok" || payment.string("payment_id") != "pay-1" ||
		payment.string("payment_status") != "paid" || payment.string("access_application_state") != "applied" ||
		payment.string("credited_entitlement_revision") != "9" {
		t.Fatalf("payment_create_result wrong: %+v", payment)
	}
	if reference, present := payment["checkout_reference"]; !present || reference != nil {
		t.Fatalf("null checkout_reference must be passed through as null: %+v", payment)
	}

	// Lost-response retry: the host re-sends the same action with the same key and the
	// same business data; native forwards the key untouched.
	harness.sendAction(bridgeMessage{"type": paymentActionPaymentCreate, "quote_id": "q-1",
		"idempotency_key": paymentKey})
	harness.waitForEventCount(paymentEventPaymentCreate, 2, 5*time.Second)
	paymentRequests := harness.requestsFor("/api/mobile/v1/payments")
	if len(paymentRequests) != 2 || paymentRequests[0].Key != paymentKey || paymentRequests[1].Key != paymentKey {
		t.Fatalf("payment retry did not keep the same host key: %+v", paymentRequests)
	}
	if paymentRequests[0].Body != `{"quote_id":"q-1"}` || paymentRequests[0].Body != paymentRequests[1].Body {
		t.Fatalf("payment retry body changed: %+v", paymentRequests)
	}

	harness.sendAction(bridgeMessage{"type": paymentActionPaymentGet, "payment_id": "pay-1"})
	read := harness.waitForEvent(paymentEventPaymentGet, 5*time.Second)
	if read.string("state") != "ok" || read.string("payment_id") != "pay-1" {
		t.Fatalf("payment_get_result wrong: %+v", read)
	}
	readRequests := harness.requestsFor("/api/mobile/v1/payments/pay-1")
	if len(readRequests) != 1 || readRequests[0].Key != "" {
		t.Fatalf("payment read must be a keyless GET: %+v", readRequests)
	}

	// No entitlement/access mutation happened on any payment path: /me was not
	// re-read, no new account_access event fired, and the in-memory verified state is
	// byte-identical to the pre-payment snapshot.
	if got := harness.countPath("/api/mobile/v1/me"); got != meBefore {
		t.Fatalf("payment handling triggered /me: %d -> %d", meBefore, got)
	}
	if got := harness.eventCount("account_access"); got != accessBefore {
		t.Fatalf("payment handling emitted account_access: %d -> %d", accessBefore, got)
	}
	harness.mobile.mu.Lock()
	me := harness.mobile.me
	harness.mobile.mu.Unlock()
	if me == nil || me.Entitlement.Revision != "3" || me.AccountState != "ACTIVE_TRIAL" {
		t.Fatalf("in-memory entitlement changed by payment handling: %+v", me)
	}
}

// TestPaymentBridgeInvalidRequestNeverReachesWire proves a malformed host action is
// answered with the bounded INVALID_REQUEST and starts no request.
func TestPaymentBridgeInvalidRequestNeverReachesWire(t *testing.T) {
	harness := newPaymentHarness(t, nil)
	harness.waitForSubstring(`"type":"account_access"`, 5*time.Second)

	harness.sendAction(bridgeMessage{"type": paymentActionQuoteCreate, "plan_id": "p30",
		"duration_code": "days:30", "method": "card", "idempotency_key": ""})
	event := harness.waitForEvent(paymentEventQuoteCreate, 5*time.Second)
	if event.string("state") != "error" || event.string("code") != paymentCodeInvalidRequest {
		t.Fatalf("empty host key not rejected: %+v", event)
	}

	harness.sendAction(bridgeMessage{"type": paymentActionPaymentGet, "payment_id": "../me"})
	event = harness.waitForEvent(paymentEventPaymentGet, 5*time.Second)
	if event.string("state") != "error" || event.string("code") != paymentCodeInvalidRequest {
		t.Fatalf("traversal id not rejected: %+v", event)
	}
	if got := harness.countPath("/api/mobile/v1/quotes"); got != 0 {
		t.Fatalf("invalid quote action reached the wire: %d", got)
	}
	if got := harness.countPath("/api/mobile/v1/me"); got != 1 {
		t.Fatalf("invalid payment action touched /me: %d", got)
	}
}

// TestPaymentBridgeProviderUnavailable pins the distinct unavailable state: a 503
// SERVICE_UNAVAILABLE on a payment action becomes PROVIDER_UNAVAILABLE (the
// PLATEGA_ENABLED=false shape), while the public plans read keeps its own bounded
// SERVICE_UNAVAILABLE code. No success is faked anywhere.
func TestPaymentBridgeProviderUnavailable(t *testing.T) {
	errorRaw := paymentBridgeFixture(t, "error_service_unavailable.json", paymentBridgeErrorFixtureSHA)
	harness := newPaymentHarness(t, func(writer http.ResponseWriter, request *http.Request) bool {
		switch request.URL.Path {
		case "/api/mobile/v1/payments", "/api/mobile/v1/quotes", "/api/mobile/v1/plans":
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write(errorRaw)
			return true
		}
		return false
	})
	harness.waitForSubstring(`"type":"account_access"`, 5*time.Second)

	harness.sendAction(bridgeMessage{"type": paymentActionPaymentCreate, "quote_id": "q-1",
		"idempotency_key": "host-key-00000000000000000000000013"})
	event := harness.waitForEvent(paymentEventPaymentCreate, 5*time.Second)
	if event.string("state") != "error" || event.string("code") != paymentCodeProviderUnavailable {
		t.Fatalf("provider outage not surfaced distinctly: %+v", event)
	}
	if harness.eventCount(paymentEventPaymentCreate) != 1 {
		t.Fatalf("provider outage produced extra payment events: %s", harness.output.String())
	}

	harness.sendAction(bridgeMessage{"type": paymentActionPlansList})
	event = harness.waitForEvent(paymentEventPlansList, 5*time.Second)
	if event.string("state") != "error" || event.string("code") != "SERVICE_UNAVAILABLE" {
		t.Fatalf("plans outage must keep its bounded contract code: %+v", event)
	}
}

// TestPaymentBridgeErrorCodeMapping is the focused bounded-vocabulary matrix: only the
// frozen errors.json codes pass through, SERVICE_UNAVAILABLE on the payment family
// collapses to the distinct PROVIDER_UNAVAILABLE, and everything else is TRANSPORT.
func TestPaymentBridgeErrorCodeMapping(t *testing.T) {
	cases := []struct {
		name     string
		apiError *accountaccess.ErrorResponse
		provider bool
		want     string
	}{
		{"nil", nil, true, paymentCodeTransport},
		{"provider outage", &accountaccess.ErrorResponse{Code: "SERVICE_UNAVAILABLE"}, true, paymentCodeProviderUnavailable},
		{"plans outage", &accountaccess.ErrorResponse{Code: "SERVICE_UNAVAILABLE"}, false, "SERVICE_UNAVAILABLE"},
		{"idempotency conflict", &accountaccess.ErrorResponse{Code: "IDEMPOTENCY_CONFLICT"}, true, "IDEMPOTENCY_CONFLICT"},
		{"payment not found", &accountaccess.ErrorResponse{Code: "PAYMENT_NOT_FOUND"}, true, "PAYMENT_NOT_FOUND"},
		{"quote expired", &accountaccess.ErrorResponse{Code: "QUOTE_EXPIRED"}, true, "QUOTE_EXPIRED"},
		{"unknown code", &accountaccess.ErrorResponse{Code: "TOTALLY_NEW_CODE"}, true, paymentCodeTransport},
	}
	for _, testCase := range cases {
		if got := paymentAPIErrorCode(testCase.apiError, testCase.provider); got != testCase.want {
			t.Fatalf("%s: got %s want %s", testCase.name, got, testCase.want)
		}
	}
	if got := paymentTransportCode(&servicechannel.ServiceError{Code: "SERVICE_UNAVAILABLE"}, true); got != paymentCodeProviderUnavailable {
		t.Fatalf("service frame outage mapped to %s", got)
	}
	if got := paymentTransportCode(errors.New("boom"), true); got != paymentCodeTransport {
		t.Fatalf("plain transport mapped to %s", got)
	}
	if got := paymentEventForAction("plans_list"); got != paymentEventPlansList {
		t.Fatalf("action mapping wrong: %s", got)
	}
	if got := paymentEventForAction("unknown_action"); got != "" {
		t.Fatalf("unknown action mapped to %s", got)
	}
}

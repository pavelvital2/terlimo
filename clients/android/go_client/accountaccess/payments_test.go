package accountaccess

// Hermetic S5 payment contract tests against the authoritative synthetic fixtures of
// contracts-3e12f6f9. The five fixtures are copied byte-identical under
// go_client/testdata/step034-payments/ and every read verifies the pinned sha256 first:
// a rebuilt or edited fixture aborts the test instead of silently fitting the
// assertion. No server, no network, no credentials.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const (
	paymentsPlansFixtureSHA    = "fb71eca0fb7938bd898d0d8fdfacef037c8eae0a62a7e56f8d75b88b1dcef438"
	paymentsQuoteFixtureSHA    = "eda35cea8097159be5b2f2145e38a8896713d2fb97b28a44ca4e8239a250e202"
	paymentsPaymentFixtureSHA  = "a61fdbf2cdc701a15cd8786dfdfbae7eb28dcedb9b43eeb288198b08790d6561"
	paymentsCheckoutFixtureSHA = "31a11bbeb803e20edcb7e0b36d5fe0bd4ff916b6c9cebd889854c457ea5c862e"
	paymentsErrorFixtureSHA    = "4fb377685516c8bc7255d8d7af9ec77dee2d49e9ff3706d2b0b010a057f25de1"
)

func paymentsFixture(t *testing.T, name, wantSHA string) []byte {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("payment fixture: runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "..", "testdata", "step034-payments", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("payment fixture %s: %v", name, err)
	}
	digest := sha256.Sum256(raw)
	got := hex.EncodeToString(digest[:])
	if got != wantSHA {
		t.Fatalf("payment fixture %s sha256 mismatch: got %s want %s; aborting", name, got, wantSHA)
	}
	return raw
}

func mutateJSON(t *testing.T, raw []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	mutate(value)
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	return out
}

func planEntry(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	plans, ok := value["plans"].([]any)
	if !ok || len(plans) != 1 {
		t.Fatalf("fixture plan list changed: %v", value["plans"])
	}
	plan, ok := plans[0].(map[string]any)
	if !ok {
		t.Fatalf("fixture plan entry changed: %v", plans[0])
	}
	return plan
}

func moneyEntry(t *testing.T, owner map[string]any) map[string]any {
	t.Helper()
	amount, ok := owner["amount"].(map[string]any)
	if !ok {
		t.Fatalf("fixture amount changed: %v", owner["amount"])
	}
	return amount
}

// TestPaymentFixturesStrictDecode pins the contract success fixtures and the bounded
// error fixture to the exact values the decoders must surface.
func TestPaymentFixturesStrictDecode(t *testing.T) {
	plans, err := DecodePlansStrict(paymentsFixture(t, "plans_response.json", paymentsPlansFixtureSHA))
	if err != nil {
		t.Fatalf("plans fixture rejected: %v", err)
	}
	if plans.RequestID != "0123456789abcdef0123456789abcdef" || plans.PlansRevision != "5" || len(plans.Plans) != 1 {
		t.Fatalf("plans projection wrong: %+v", plans)
	}
	plan := plans.Plans[0]
	if plan.PlanID != "p30" || plan.Title != "30 дней" || plan.DurationCode != "days:30" ||
		plan.BaseDeviceLimit != 2 || plan.Amount.AmountMinor != 30000 || plan.Amount.Currency != "RUB" {
		t.Fatalf("plan projection wrong: %+v", plan)
	}
	if len(plan.Methods) != 2 || plan.Methods[0] != "card" || plan.Methods[1] != "sbp" {
		t.Fatalf("plan methods wrong: %v", plan.Methods)
	}

	quote, err := DecodeQuoteStrict(paymentsFixture(t, "quote_response.json", paymentsQuoteFixtureSHA))
	if err != nil {
		t.Fatalf("quote fixture rejected: %v", err)
	}
	if quote.QuoteID != "q-1" || quote.DurationCode != "days:30" || quote.DeviceLimit != 2 ||
		quote.Method != "card" || quote.ExpiresAt != "2026-09-19T15:35:00Z" ||
		quote.Amount.AmountMinor != 30000 || quote.Amount.Currency != "RUB" {
		t.Fatalf("quote projection wrong: %+v", quote)
	}

	payment, err := DecodePaymentStrict(paymentsFixture(t, "payment_response.json", paymentsPaymentFixtureSHA))
	if err != nil {
		t.Fatalf("payment fixture rejected: %v", err)
	}
	if payment.PaymentID != "pay-1" || payment.PaymentStatus != "paid" ||
		payment.AccessApplicationState != "applied" || payment.CheckoutReference != nil ||
		payment.CreditedEntitlementRevision == nil || *payment.CreditedEntitlementRevision != "9" {
		t.Fatalf("payment projection wrong: %+v", payment)
	}

	session, err := DecodeCheckoutSessionStrict(paymentsFixture(t, "checkout_session_response.json", paymentsCheckoutFixtureSHA))
	if err != nil {
		t.Fatalf("checkout fixture rejected: %v", err)
	}
	if session.CheckoutSessionID != "cs-1" || session.PolicyVersion != "checkout-policy-1" ||
		session.ExpiresAt != "2026-09-19T15:30:00Z" ||
		len(session.AllowedOrigins) != 1 || session.AllowedOrigins[0] != "https://pay.example.test" ||
		len(session.AllowedRedirects) != 1 || session.AllowedRedirects[0] != "https://bank.example.test/3ds" {
		t.Fatalf("checkout projection wrong: %+v", session)
	}

	envelope, err := DecodeErrorStrict(paymentsFixture(t, "error_service_unavailable.json", paymentsErrorFixtureSHA))
	if err != nil {
		t.Fatalf("error fixture rejected: %v", err)
	}
	if envelope.Code != "SERVICE_UNAVAILABLE" || !envelope.Retryable {
		t.Fatalf("error fixture projection wrong: %+v", envelope)
	}
}

// TestPaymentStrictRejections is the unknown-field/enum/required/bounds matrix: every
// mutated body must be rejected, never partially accepted.
func TestPaymentStrictRejections(t *testing.T) {
	type decodeCase struct {
		name    string
		fixture string
		sha     string
		decode  func([]byte) error
		mutate  func(map[string]any)
	}
	cases := []decodeCase{
		{"plans unknown field", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { value["qr_payload"] = "data:image/png;base64,AAAA" }},
		{"plans schema_version", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { value["schema_version"] = "2.0" }},
		{"plans status", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { value["status"] = "error" }},
		{"plans request_id", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { value["request_id"] = "not-hex" }},
		{"plans revision", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { value["plans_revision"] = "05" }},
		{"plans revision missing", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { delete(value, "plans_revision") }},
		{"plans null list", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { value["plans"] = nil }},
		{"plan duration enum", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { planEntry(t, value)["duration_code"] = "days:31" }},
		{"plan amount float", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { moneyEntry(t, planEntry(t, value))["amount_minor"] = 30000.5 }},
		{"plan amount negative", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { moneyEntry(t, planEntry(t, value))["amount_minor"] = -1 }},
		{"plan amount_minor missing", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { delete(moneyEntry(t, planEntry(t, value)), "amount_minor") }},
		{"plan currency pattern", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { moneyEntry(t, planEntry(t, value))["currency"] = "rub" }},
		{"plan duplicate method", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { planEntry(t, value)["methods"] = []any{"card", "card"} }},
		{"plan method enum", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { planEntry(t, value)["methods"] = []any{"cash"} }},
		{"plan methods null", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { planEntry(t, value)["methods"] = nil }},
		{"plan device limit", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { planEntry(t, value)["base_device_limit"] = 0 }},
		{"plan amount extra field", "plans_response.json", paymentsPlansFixtureSHA,
			func(raw []byte) error { _, err := DecodePlansStrict(raw); return err },
			func(value map[string]any) { moneyEntry(t, planEntry(t, value))["amount_float"] = 300.0 }},
		{"quote device_limit", "quote_response.json", paymentsQuoteFixtureSHA,
			func(raw []byte) error { _, err := DecodeQuoteStrict(raw); return err },
			func(value map[string]any) { value["device_limit"] = 0 }},
		{"quote_id empty", "quote_response.json", paymentsQuoteFixtureSHA,
			func(raw []byte) error { _, err := DecodeQuoteStrict(raw); return err },
			func(value map[string]any) { value["quote_id"] = "" }},
		{"quote expires_at missing", "quote_response.json", paymentsQuoteFixtureSHA,
			func(raw []byte) error { _, err := DecodeQuoteStrict(raw); return err },
			func(value map[string]any) { delete(value, "expires_at") }},
		{"quote unknown field", "quote_response.json", paymentsQuoteFixtureSHA,
			func(raw []byte) error { _, err := DecodeQuoteStrict(raw); return err },
			func(value map[string]any) { value["checkout_url"] = "https://evil.test/pay" }},
		{"payment status enum", "payment_response.json", paymentsPaymentFixtureSHA,
			func(raw []byte) error { _, err := DecodePaymentStrict(raw); return err },
			func(value map[string]any) { value["payment_status"] = "processing" }},
		{"payment access state enum", "payment_response.json", paymentsPaymentFixtureSHA,
			func(raw []byte) error { _, err := DecodePaymentStrict(raw); return err },
			func(value map[string]any) { value["access_application_state"] = "granted" }},
		{"payment checkout_reference bound", "payment_response.json", paymentsPaymentFixtureSHA,
			func(raw []byte) error { _, err := DecodePaymentStrict(raw); return err },
			func(value map[string]any) { value["checkout_reference"] = strings.Repeat("r", 257) }},
		{"payment_id empty", "payment_response.json", paymentsPaymentFixtureSHA,
			func(raw []byte) error { _, err := DecodePaymentStrict(raw); return err },
			func(value map[string]any) { value["payment_id"] = "" }},
		{"payment unknown field", "payment_response.json", paymentsPaymentFixtureSHA,
			func(raw []byte) error { _, err := DecodePaymentStrict(raw); return err },
			func(value map[string]any) { value["qr_svg"] = "<svg/>" }},
		{"checkout origins bound", "checkout_session_response.json", paymentsCheckoutFixtureSHA,
			func(raw []byte) error { _, err := DecodeCheckoutSessionStrict(raw); return err },
			func(value map[string]any) {
				origins := make([]any, 33)
				for index := range origins {
					origins[index] = "https://pay.example.test"
				}
				value["allowed_origins"] = origins
			}},
		{"checkout redirects null", "checkout_session_response.json", paymentsCheckoutFixtureSHA,
			func(raw []byte) error { _, err := DecodeCheckoutSessionStrict(raw); return err },
			func(value map[string]any) { value["allowed_redirects"] = nil }},
		{"checkout policy_version bound", "checkout_session_response.json", paymentsCheckoutFixtureSHA,
			func(raw []byte) error { _, err := DecodeCheckoutSessionStrict(raw); return err },
			func(value map[string]any) { value["policy_version"] = strings.Repeat("p", 65) }},
		{"checkout session id empty", "checkout_session_response.json", paymentsCheckoutFixtureSHA,
			func(raw []byte) error { _, err := DecodeCheckoutSessionStrict(raw); return err },
			func(value map[string]any) { value["checkout_session_id"] = "" }},
	}
	for _, testCase := range cases {
		raw := paymentsFixture(t, testCase.fixture, testCase.sha)
		mutated := mutateJSON(t, raw, testCase.mutate)
		if err := testCase.decode(mutated); err == nil {
			t.Fatalf("%s: mutated body accepted: %s", testCase.name, mutated)
		}
	}
}

// capturedPaymentRequest is one bounded request seen by the local test server.
type capturedPaymentRequest struct {
	Method string
	Path   string
	Key    string
	Auth   string
	Body   string
}

type paymentCapture struct {
	mu       sync.Mutex
	requests []capturedPaymentRequest
	respond  func(w http.ResponseWriter, r *http.Request) bool
}

func (c *paymentCapture) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		c.mu.Lock()
		c.requests = append(c.requests, capturedPaymentRequest{
			Method: request.Method, Path: request.URL.Path,
			Key: request.Header.Get("Idempotency-Key"), Auth: request.Header.Get("Authorization"),
			Body: string(body),
		})
		respond := c.respond
		c.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if respond == nil || !respond(writer, request) {
			writer.WriteHeader(http.StatusNotFound)
		}
	})
}

func (c *paymentCapture) snapshot() []capturedPaymentRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedPaymentRequest(nil), c.requests...)
}

func (c *paymentCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func paymentTestClient(t *testing.T, capture *paymentCapture) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(capture.handler())
	t.Cleanup(server.Close)
	client := &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: StaticToken("test-bearer")}
	return client, server
}

func writeFixture(writer http.ResponseWriter, raw []byte) {
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(raw)
}

// TestPaymentClientForwardsHostIdempotencyKeyVerbatimOnRetry proves the host-owned key
// travels byte-for-byte on every attempt and retry (same key, same business data) and
// that the client never invents or rotates a key.
func TestPaymentClientForwardsHostIdempotencyKeyVerbatimOnRetry(t *testing.T) {
	plansRaw := paymentsFixture(t, "plans_response.json", paymentsPlansFixtureSHA)
	quoteRaw := paymentsFixture(t, "quote_response.json", paymentsQuoteFixtureSHA)
	paymentRaw := paymentsFixture(t, "payment_response.json", paymentsPaymentFixtureSHA)
	checkoutRaw := paymentsFixture(t, "checkout_session_response.json", paymentsCheckoutFixtureSHA)
	capture := &paymentCapture{respond: func(writer http.ResponseWriter, request *http.Request) bool {
		switch request.URL.Path {
		case "/api/mobile/v1/plans":
			writeFixture(writer, plansRaw)
		case "/api/mobile/v1/quotes":
			writeFixture(writer, quoteRaw)
		case "/api/mobile/v1/payments":
			writeFixture(writer, paymentRaw)
		case "/api/mobile/v1/payments/pay-1":
			writeFixture(writer, paymentRaw)
		case "/api/mobile/v1/payments/pay-1/checkout-session":
			writeFixture(writer, checkoutRaw)
		default:
			return false
		}
		return true
	}}
	client, _ := paymentTestClient(t, capture)
	ctx := context.Background()

	const quoteKey = "host-key-00000000000000000000000001"
	const paymentKey = "host-key-00000000000000000000000002"
	// A re-sent lost-response retry: identical key AND identical business data.
	for attempt := 0; attempt < 2; attempt++ {
		if _, _, err := client.CreateQuote(ctx, "p30", "days:30", "card", quoteKey); err != nil {
			t.Fatalf("quote attempt %d: %v", attempt, err)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, _, err := client.CreatePayment(ctx, "q-1", paymentKey); err != nil {
			t.Fatalf("payment attempt %d: %v", attempt, err)
		}
	}
	if _, _, err := client.CreateCheckoutSession(ctx, "pay-1", quoteKey); err != nil {
		t.Fatalf("checkout session: %v", err)
	}
	if _, _, err := client.GetPayment(ctx, "pay-1"); err != nil {
		t.Fatalf("payment get: %v", err)
	}

	requests := capture.snapshot()
	if len(requests) != 6 {
		t.Fatalf("expected 6 captured requests, got %d: %+v", len(requests), requests)
	}
	for index, request := range requests {
		if request.Auth != "Bearer test-bearer" {
			t.Fatalf("request %d lost the bearer: %+v", index, request)
		}
	}
	for _, request := range requests[:2] {
		if request.Key != quoteKey {
			t.Fatalf("quote retry key changed: %+v", request)
		}
	}
	if requests[0].Body != requests[1].Body {
		t.Fatalf("quote retry body changed: %q vs %q", requests[0].Body, requests[1].Body)
	}
	if requests[0].Body != `{"plan_id":"p30","duration_code":"days:30","method":"card"}` {
		t.Fatalf("quote body not contract-shaped: %q", requests[0].Body)
	}
	for _, request := range requests[2:4] {
		if request.Key != paymentKey {
			t.Fatalf("payment retry key changed: %+v", request)
		}
	}
	if requests[2].Body != `{"quote_id":"q-1"}` {
		t.Fatalf("payment body not contract-shaped: %q", requests[2].Body)
	}
	if requests[4].Key != quoteKey || requests[4].Path != "/api/mobile/v1/payments/pay-1/checkout-session" {
		t.Fatalf("checkout request lost the host key: %+v", requests[4])
	}
	if requests[5].Key != "" || requests[5].Path != "/api/mobile/v1/payments/pay-1" {
		t.Fatalf("payment read carried a key or wrong path: %+v", requests[5])
	}
}

// TestPaymentClientRejectsInvalidHostInputWithoutRequest proves a missing or malformed
// host key/field is a bounded local INVALID_REQUEST with zero network attempts: native
// rejects instead of generating a key.
func TestPaymentClientRejectsInvalidHostInputWithoutRequest(t *testing.T) {
	capture := &paymentCapture{}
	client, _ := paymentTestClient(t, capture)
	ctx := context.Background()
	validKey := "host-key-00000000000000000000000003"

	cases := []struct {
		name string
		call func() error
	}{
		{"quote empty key", func() error {
			_, _, err := client.CreateQuote(ctx, "p30", "days:30", "card", "")
			return err
		}},
		{"quote short key", func() error {
			_, _, err := client.CreateQuote(ctx, "p30", "days:30", "card", "too-short")
			return err
		}},
		{"quote crlf key", func() error {
			_, _, err := client.CreateQuote(ctx, "p30", "days:30", "card", "host-key-12345678\r\nX: 1")
			return err
		}},
		{"quote duration", func() error {
			_, _, err := client.CreateQuote(ctx, "p30", "days:31", "card", validKey)
			return err
		}},
		{"quote method", func() error {
			_, _, err := client.CreateQuote(ctx, "p30", "days:30", "cash", validKey)
			return err
		}},
		{"quote blank plan", func() error {
			_, _, err := client.CreateQuote(ctx, "", "days:30", "card", validKey)
			return err
		}},
		{"payment blank quote", func() error {
			_, _, err := client.CreatePayment(ctx, "", validKey)
			return err
		}},
		{"payment empty key", func() error {
			_, _, err := client.CreatePayment(ctx, "q-1", "")
			return err
		}},
		{"get blank id", func() error {
			_, _, err := client.GetPayment(ctx, "")
			return err
		}},
		{"get traversal id", func() error {
			_, _, err := client.GetPayment(ctx, "../me")
			return err
		}},
		{"get slash id", func() error {
			_, _, err := client.GetPayment(ctx, "pay-1/checkout-session")
			return err
		}},
		{"checkout blank id", func() error {
			_, _, err := client.CreateCheckoutSession(ctx, "", validKey)
			return err
		}},
		{"checkout empty key", func() error {
			_, _, err := client.CreateCheckoutSession(ctx, "pay-1", "")
			return err
		}},
	}
	for _, testCase := range cases {
		if err := testCase.call(); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s: expected ErrInvalidRequest, got %v", testCase.name, err)
		}
	}
	if capture.count() != 0 {
		t.Fatalf("invalid host input reached the wire: %+v", capture.snapshot())
	}
	if validKey != strings.TrimSpace(validKey) || len(validKey) < 16 {
		t.Fatal("test key helper broken")
	}
}

// TestPaymentClientPublicPlansIsBearerless proves /plans is read exactly as the public
// contract route: no Authorization header and no TokenSource consultation.
func TestPaymentClientPublicPlansIsBearerless(t *testing.T) {
	plansRaw := paymentsFixture(t, "plans_response.json", paymentsPlansFixtureSHA)
	capture := &paymentCapture{respond: func(writer http.ResponseWriter, request *http.Request) bool {
		if request.URL.Path != "/api/mobile/v1/plans" {
			return false
		}
		writeFixture(writer, plansRaw)
		return true
	}}
	server := httptest.NewServer(capture.handler())
	defer server.Close()
	client := &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: StaticToken("test-bearer")}
	plans, apiError, err := client.ListPlans(context.Background())
	if err != nil || apiError != nil {
		t.Fatalf("plans: %v %v", apiError, err)
	}
	if len(plans.Plans) != 1 || plans.Plans[0].PlanID != "p30" {
		t.Fatalf("plans projection wrong: %+v", plans)
	}
	requests := capture.snapshot()
	if len(requests) != 1 || requests[0].Auth != "" || requests[0].Method != http.MethodGet {
		t.Fatalf("plans request was not public GET: %+v", requests)
	}
}

// TestPaymentClientServiceUnavailableAPIError proves the bounded 503 envelope survives
// as a typed API error (the bridge turns it into the provider-unavailable state).
func TestPaymentClientServiceUnavailableAPIError(t *testing.T) {
	errorRaw := paymentsFixture(t, "error_service_unavailable.json", paymentsErrorFixtureSHA)
	capture := &paymentCapture{respond: func(writer http.ResponseWriter, request *http.Request) bool {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write(errorRaw)
		return true
	}}
	client, _ := paymentTestClient(t, capture)
	_, apiError, err := client.CreatePayment(context.Background(), "q-1", "host-key-00000000000000000000000004")
	if err != nil || apiError == nil {
		t.Fatalf("expected a typed API error, got %v %v", apiError, err)
	}
	if apiError.Code != "SERVICE_UNAVAILABLE" || !apiError.Retryable {
		t.Fatalf("error envelope changed: %+v", apiError)
	}
}

// TestPaymentClientEmptySuccessListIsNotAnErrorMatrix pins that an empty plans array
// and a null checkout_reference are accepted as real successful responses (no
// fabricated rows, no fabricated reference).
func TestPaymentClientEmptySuccessListIsNotAnErrorMatrix(t *testing.T) {
	plansRaw := mutateJSON(t, paymentsFixture(t, "plans_response.json", paymentsPlansFixtureSHA), func(value map[string]any) {
		value["plans"] = []any{}
	})
	plans, err := DecodePlansStrict(plansRaw)
	if err != nil || plans.Plans == nil || len(plans.Plans) != 0 {
		t.Fatalf("empty plans list must decode as a real response: %+v %v", plans, err)
	}

	paymentRaw := paymentsFixture(t, "payment_response.json", paymentsPaymentFixtureSHA)
	if !bytes.Contains(paymentRaw, []byte(`"checkout_reference": null`)) {
		t.Fatal("fixture no longer pins the null checkout_reference")
	}
	payment, err := DecodePaymentStrict(paymentRaw)
	if err != nil || payment.CheckoutReference != nil {
		t.Fatalf("null checkout_reference must stay null: %+v %v", payment, err)
	}
	if payment.CreditedEntitlementRevision == nil {
		t.Fatal("credited revision must be passed through, not dropped")
	}
}

func TestPaymentDecodersRejectTrailingJSON(t *testing.T) {
	plansRaw := paymentsFixture(t, "plans_response.json", paymentsPlansFixtureSHA)
	trailing := append(append([]byte(nil), plansRaw...), []byte(` {"second":true}`)...)
	if _, err := DecodePlansStrict(trailing); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	quoteRaw := paymentsFixture(t, "quote_response.json", paymentsQuoteFixtureSHA)
	if _, err := DecodeQuoteStrict(fmt.Appendf(nil, "%snull", quoteRaw)); err == nil {
		t.Fatal("trailing null accepted")
	}
}

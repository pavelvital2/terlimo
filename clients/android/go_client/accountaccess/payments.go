package accountaccess

// STEP03.4 payment surface: strict plans/quote/payment/checkout-session contract
// consumption on the same client/transport seam as the account-access routes
// (contract basis contracts-3e12f6f9: openapi/mobile_v1.yaml paths /plans GET with optional authentication,
// /quotes POST, /payments POST, /payments/{id} GET, /payments/{id}/checkout-session POST
// and schemas/payment.json + schemas/common.json).
//
// Native is only a bounded forwarder here. It never fabricates QR data (this contract
// carries no QR field): `checkout_reference` and the checkout capability data are
// forwarded verbatim or as null. The host owns every Idempotency-Key; native forwards
// the exact bytes in the `Idempotency-Key` header on every attempt/retry and never
// generates, rotates, reuses for changed data or drops one. No payment/redirect path
// can mutate an entitlement: entitlement changes arrive only through a fresh /me read.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// ErrInvalidRequest marks bounded native input (a host action field) that does not
// match the frozen contract before any request is built. It is never a server
// response classification and never triggers a retry.
var ErrInvalidRequest = errors.New("INVALID_REQUEST")

var (
	currencyPattern      = regexp.MustCompile(`^[A-Z]{3}$`)
	paymentPathIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)
)

// validOpaqueID is the contract OpaqueId: 1..128 characters.
func validOpaqueID(value string) bool { return value != "" && len(value) <= 128 }

func validDurationCode(value string) bool {
	switch value {
	case "days:30", "months:3", "months:6":
		return true
	}
	return false
}

func validPaymentMethod(value string) bool {
	switch value {
	case "card", "sbp", "crypto":
		return true
	}
	return false
}

func validPaymentStatus(value string) bool {
	switch value {
	case "created", "pending", "paid", "failed", "expired", "refunded", "disputed":
		return true
	}
	return false
}

func validAccessApplicationState(value string) bool {
	switch value {
	case "not_requested", "pending", "applied", "retryable_failure", "rejected":
		return true
	}
	return false
}

// validPaymentPathID bounds a /payments/{id} path segment exactly like the service
// channel allowlist: one bounded segment, no slash/dot-dot/percent trick. A payment id
// that is not a legal single path segment must never be concatenated into a URL (an id
// containing "/" would otherwise alias another allowlisted route).
func validPaymentPathID(value string) bool {
	if value == "" || value == "." || value == ".." || strings.Contains(value, "..") {
		return false
	}
	return paymentPathIDPattern.MatchString(value)
}

// validIdempotencyKey bounds the host-owned key shape from the contract (16..128) and
// forbids header injection. Native forwards the exact bytes: it never rewrites a key,
// and an absent/blank key is rejected instead of generated.
func validIdempotencyKey(value string) bool {
	return len(value) >= 16 && len(value) <= 128 && !strings.ContainsAny(value, "\r\n")
}

// unmarshalStrictRequired decodes one closed contract object (unknown fields rejected)
// and additionally proves that every listed key was literally present. It exists
// because a required scalar can fold into a zero value that still passes a range check
// (for example money amount_minor 0, which is itself a valid amount).
func unmarshalStrictRequired(raw []byte, out any, required ...string) error {
	if err := decodeStrict(raw, out); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	for _, key := range required {
		if _, present := keys[key]; !present {
			return fmt.Errorf("required field %q missing", key)
		}
	}
	return nil
}

// Money is the contract exact integer minor-units amount; no floating point money.
type Money struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// UnmarshalJSON enforces the required amount_minor/currency presence.
func (m *Money) UnmarshalJSON(raw []byte) error {
	type plain Money
	var value plain
	if err := unmarshalStrictRequired(raw, &value, "amount_minor", "currency"); err != nil {
		return err
	}
	*m = Money(value)
	return nil
}

// Plan is one GET /plans entry.
type Plan struct {
	PlanID          string   `json:"plan_id"`
	Title           string   `json:"title"`
	DurationCode    string   `json:"duration_code"`
	BaseDeviceLimit int      `json:"base_device_limit"`
	Amount          Money    `json:"amount"`
	Methods         []string `json:"methods"`
	Product         *Product `json:"-"`
	PaymentContract int      `json:"-"`
}

// UnmarshalJSON enforces the required plan field presence.
func (p *Plan) UnmarshalJSON(raw []byte) error {
	type plain Plan
	var value plain
	if err := unmarshalStrictRequired(raw, &value,
		"plan_id", "title", "duration_code", "base_device_limit", "amount", "methods"); err != nil {
		return err
	}
	*p = Plan(value)
	return nil
}

// PlansResponse is the strict GET /plans 200 body, identical with or without authentication.
type PlansResponse struct {
	RequestID     string `json:"request_id"`
	ServerTime    string `json:"server_time"`
	SchemaVersion string `json:"schema_version"`
	Status        string `json:"status"`
	Plans         []Plan `json:"plans"`
	PlansRevision string `json:"plans_revision"`
}

// UnmarshalJSON enforces the required response field presence.
func (p *PlansResponse) UnmarshalJSON(raw []byte) error {
	type plain PlansResponse
	var value plain
	if err := unmarshalStrictRequired(raw, &value,
		"request_id", "server_time", "schema_version", "status", "plans", "plans_revision"); err != nil {
		return err
	}
	*p = PlansResponse(value)
	return nil
}

// QuoteRequest is the POST /quotes body; Idempotency is header-only.
type QuoteRequest struct {
	PlanID            string   `json:"plan_id"`
	DurationCode      string   `json:"duration_code"`
	Method            string   `json:"method"`
	RenewExtraSlotIDs []string `json:"renew_extra_slot_ids,omitempty"`
}

// QuoteResponse is the strict POST /quotes 200 body. duration_code, method and
// expires_at are plain required strings in payment.json (only the request carries the
// enums), so they are forwarded without an invented constraint.
type QuoteResponse struct {
	RequestID       string          `json:"request_id"`
	ServerTime      string          `json:"server_time"`
	SchemaVersion   string          `json:"schema_version"`
	Status          string          `json:"status"`
	QuoteID         string          `json:"quote_id"`
	Amount          Money           `json:"amount"`
	DurationCode    string          `json:"duration_code"`
	DeviceLimit     int             `json:"device_limit"`
	Method          string          `json:"method"`
	ExpiresAt       string          `json:"expires_at"`
	Product         *Product        `json:"-"`
	Pricing         *PaymentPricing `json:"-"`
	PaymentContract int             `json:"-"`
}

// UnmarshalJSON enforces the required response field presence.
func (q *QuoteResponse) UnmarshalJSON(raw []byte) error {
	type plain QuoteResponse
	var value plain
	if err := unmarshalStrictRequired(raw, &value,
		"request_id", "server_time", "schema_version", "status", "quote_id", "amount",
		"duration_code", "device_limit", "method", "expires_at"); err != nil {
		return err
	}
	*q = QuoteResponse(value)
	return nil
}

// PaymentCreateRequest is the POST /payments body; Idempotency is header-only.
type PaymentCreateRequest struct {
	QuoteID string `json:"quote_id"`
}

// PaymentResponse is the strict POST /payments and GET /payments/{id} 200 body.
// checkout_reference and credited_entitlement_revision are optional nullable fields:
// they are passed through verbatim (or nil) and never synthesized into a grant.
type PaymentResponse struct {
	RequestID                   string           `json:"request_id"`
	ServerTime                  string           `json:"server_time"`
	SchemaVersion               string           `json:"schema_version"`
	Status                      string           `json:"status"`
	PaymentID                   string           `json:"payment_id"`
	PaymentStatus               string           `json:"payment_status"`
	CheckoutReference           *string          `json:"checkout_reference"`
	CreditedEntitlementRevision *string          `json:"credited_entitlement_revision"`
	AccessApplicationState      string           `json:"access_application_state"`
	Product                     *Product         `json:"-"`
	CreditState                 string           `json:"-"`
	CreditReviewReason          *string          `json:"-"`
	CreditedProduct             *CreditedProduct `json:"-"`
	PaymentContract             int              `json:"-"`
	Pricing                     *PaymentPricing  `json:"-"`
	ReferralDiscountState       string           `json:"-"`
}

// UnmarshalJSON enforces the required response field presence.
func (p *PaymentResponse) UnmarshalJSON(raw []byte) error {
	type plain PaymentResponse
	var value plain
	if err := unmarshalStrictRequired(raw, &value,
		"request_id", "server_time", "schema_version", "status", "payment_id",
		"payment_status", "access_application_state"); err != nil {
		return err
	}
	*p = PaymentResponse(value)
	return nil
}

// CheckoutSessionResponse is the strict POST /payments/{id}/checkout-session 200 body.
// The hosted-checkout reserve is optional and unused by default.
type CheckoutSessionResponse struct {
	RequestID         string   `json:"request_id"`
	ServerTime        string   `json:"server_time"`
	SchemaVersion     string   `json:"schema_version"`
	Status            string   `json:"status"`
	CheckoutSessionID string   `json:"checkout_session_id"`
	PolicyVersion     string   `json:"policy_version"`
	ExpiresAt         string   `json:"expires_at"`
	AllowedOrigins    []string `json:"allowed_origins"`
	AllowedRedirects  []string `json:"allowed_redirects"`
}

// UnmarshalJSON enforces the required response field presence.
func (s *CheckoutSessionResponse) UnmarshalJSON(raw []byte) error {
	type plain CheckoutSessionResponse
	var value plain
	if err := unmarshalStrictRequired(raw, &value,
		"request_id", "server_time", "schema_version", "status", "checkout_session_id",
		"policy_version", "expires_at", "allowed_origins", "allowed_redirects"); err != nil {
		return err
	}
	*s = CheckoutSessionResponse(value)
	return nil
}

func validServerEnvelope(requestID, serverTime, schemaVersion, status string) bool {
	return schemaVersion == SchemaVersion && status == "ok" &&
		requestIDPattern.MatchString(requestID) && ValidUtcTime(serverTime)
}

func validateMoney(m Money) error {
	if m.AmountMinor < 0 {
		return fmt.Errorf("amount_minor out of range")
	}
	if !currencyPattern.MatchString(m.Currency) {
		return fmt.Errorf("currency invalid")
	}
	return nil
}

func validatePlan(plan Plan) error {
	if !validOpaqueID(plan.PlanID) {
		return fmt.Errorf("plan_id invalid")
	}
	if len(plan.Title) > 128 {
		return fmt.Errorf("title too long")
	}
	if !validDurationCode(plan.DurationCode) {
		return fmt.Errorf("duration_code invalid")
	}
	if plan.BaseDeviceLimit < 1 || plan.BaseDeviceLimit > 100 {
		return fmt.Errorf("base_device_limit out of range")
	}
	if err := validateMoney(plan.Amount); err != nil {
		return err
	}
	if plan.Methods == nil {
		return fmt.Errorf("methods missing")
	}
	seen := map[string]bool{}
	for _, method := range plan.Methods {
		if !validPaymentMethod(method) || seen[method] {
			return fmt.Errorf("methods invalid")
		}
		seen[method] = true
	}
	return nil
}

// DecodePlansStrict validates a GET /plans 200 body.
func DecodePlansStrict(raw []byte) (PlansResponse, error) {
	var response PlansResponse
	if err := decodeStrict(raw, &response); err != nil {
		return PlansResponse{}, fmt.Errorf("plans decode: %w", err)
	}
	if !validServerEnvelope(response.RequestID, response.ServerTime, response.SchemaVersion, response.Status) {
		return PlansResponse{}, fmt.Errorf("plans envelope invalid")
	}
	if !ValidRevision(response.PlansRevision) {
		return PlansResponse{}, fmt.Errorf("plans_revision invalid")
	}
	if response.Plans == nil {
		return PlansResponse{}, fmt.Errorf("plans missing")
	}
	for _, plan := range response.Plans {
		if err := validatePlan(plan); err != nil {
			return PlansResponse{}, fmt.Errorf("plan invalid: %w", err)
		}
	}
	return response, nil
}

// DecodeQuoteStrict validates a POST /quotes 200 body.
func DecodeQuoteStrict(raw []byte) (QuoteResponse, error) {
	var response QuoteResponse
	if err := decodeStrict(raw, &response); err != nil {
		return QuoteResponse{}, fmt.Errorf("quote decode: %w", err)
	}
	if !validServerEnvelope(response.RequestID, response.ServerTime, response.SchemaVersion, response.Status) {
		return QuoteResponse{}, fmt.Errorf("quote envelope invalid")
	}
	if !validOpaqueID(response.QuoteID) {
		return QuoteResponse{}, fmt.Errorf("quote_id invalid")
	}
	if err := validateMoney(response.Amount); err != nil {
		return QuoteResponse{}, fmt.Errorf("quote amount invalid")
	}
	if response.DeviceLimit < 1 || response.DeviceLimit > 100 {
		return QuoteResponse{}, fmt.Errorf("quote device_limit out of range")
	}
	return response, nil
}

// DecodePaymentStrict validates a POST /payments or GET /payments/{id} 200 body.
func DecodePaymentStrict(raw []byte) (PaymentResponse, error) {
	var response PaymentResponse
	if err := decodeStrict(raw, &response); err != nil {
		return PaymentResponse{}, fmt.Errorf("payment decode: %w", err)
	}
	if !validServerEnvelope(response.RequestID, response.ServerTime, response.SchemaVersion, response.Status) {
		return PaymentResponse{}, fmt.Errorf("payment envelope invalid")
	}
	if !validOpaqueID(response.PaymentID) {
		return PaymentResponse{}, fmt.Errorf("payment_id invalid")
	}
	if !validPaymentStatus(response.PaymentStatus) {
		return PaymentResponse{}, fmt.Errorf("payment_status enum invalid")
	}
	if !validAccessApplicationState(response.AccessApplicationState) {
		return PaymentResponse{}, fmt.Errorf("access_application_state enum invalid")
	}
	if response.CheckoutReference != nil && len(*response.CheckoutReference) > 256 {
		return PaymentResponse{}, fmt.Errorf("checkout_reference too long")
	}
	return response, nil
}

// DecodeCheckoutSessionStrict validates a POST /payments/{id}/checkout-session 200 body.
func DecodeCheckoutSessionStrict(raw []byte) (CheckoutSessionResponse, error) {
	var response CheckoutSessionResponse
	if err := decodeStrict(raw, &response); err != nil {
		return CheckoutSessionResponse{}, fmt.Errorf("checkout session decode: %w", err)
	}
	if !validServerEnvelope(response.RequestID, response.ServerTime, response.SchemaVersion, response.Status) {
		return CheckoutSessionResponse{}, fmt.Errorf("checkout session envelope invalid")
	}
	if !validOpaqueID(response.CheckoutSessionID) {
		return CheckoutSessionResponse{}, fmt.Errorf("checkout_session_id invalid")
	}
	if len(response.PolicyVersion) > 64 {
		return CheckoutSessionResponse{}, fmt.Errorf("policy_version too long")
	}
	if response.AllowedOrigins == nil || len(response.AllowedOrigins) > 32 {
		return CheckoutSessionResponse{}, fmt.Errorf("allowed_origins invalid")
	}
	if response.AllowedRedirects == nil || len(response.AllowedRedirects) > 32 {
		return CheckoutSessionResponse{}, fmt.Errorf("allowed_redirects invalid")
	}
	return response, nil
}

// ListPlans reads the bounded GET /plans list with optional authentication. An existing
// TokenSource supplies the session bearer (and account-specific prices); without one
// the request stays public. Token errors propagate, never falling back to public prices.
func (c *Client) ListPlans(ctx context.Context) (PlansResponse, *ErrorResponse, error) {
	raw, status, err := c.requestWithQuery(ctx, http.MethodGet, "/plans", nil, "", true, c.paymentQuery())
	if err != nil {
		return PlansResponse{}, nil, err
	}
	if status == http.StatusOK {
		plans, err := c.decodePlans(raw)
		if err != nil {
			return PlansResponse{}, nil, err
		}
		return plans, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return PlansResponse{}, nil, decodeErr
	}
	return PlansResponse{}, envelope, nil
}

// CreateQuote computes one immutable stored quote POST /quotes. plan_id/duration_code/
// method are validated against the request schema; the host-owned Idempotency-Key is
// forwarded verbatim in the header.
func (c *Client) CreateQuote(ctx context.Context, planID, durationCode, method, idempotencyKey string) (QuoteResponse, *ErrorResponse, error) {
	return c.CreateQuoteWithSelection(ctx, planID, durationCode, method, idempotencyKey, nil)
}

// CreateQuoteWithSelection forwards server-owned extra slot IDs, never device IDs.
func (c *Client) CreateQuoteWithSelection(ctx context.Context, planID, durationCode, method, idempotencyKey string, renewExtraSlotIDs []string) (QuoteResponse, *ErrorResponse, error) {
	validPlanDuration := validOpaqueID(planID) && validDurationCode(durationCode)
	if c.PaymentContract == 2 {
		validPlanDuration = validProductPlanID(planID) && validDurationCodeV2(durationCode)
		if !validSlotIDs(renewExtraSlotIDs) || (planID == "terlimo-extra-device" && len(renewExtraSlotIDs) != 0) {
			return QuoteResponse{}, nil, ErrInvalidRequest
		}
	} else if len(renewExtraSlotIDs) != 0 {
		return QuoteResponse{}, nil, ErrInvalidRequest
	}
	if !validPlanDuration || !validPaymentMethod(method) ||
		!validIdempotencyKey(idempotencyKey) {
		return QuoteResponse{}, nil, ErrInvalidRequest
	}
	body := QuoteRequest{PlanID: planID, DurationCode: durationCode, Method: method}
	if c.PaymentContract == 2 {
		body.RenewExtraSlotIDs = append([]string(nil), renewExtraSlotIDs...)
	}
	raw, status, err := c.requestWithQuery(ctx, http.MethodPost, "/quotes", body, idempotencyKey, true, c.paymentQuery())
	if err != nil {
		return QuoteResponse{}, nil, err
	}
	if status == http.StatusOK {
		quote, err := c.decodeQuote(raw)
		if err != nil {
			return QuoteResponse{}, nil, err
		}
		if c.PaymentContract == 2 && !quoteMatchesV2Request(quote, body) {
			return QuoteResponse{}, nil, fmt.Errorf("quote v2 request binding mismatch")
		}
		return quote, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return QuoteResponse{}, nil, decodeErr
	}
	return QuoteResponse{}, envelope, nil
}

// CreatePayment creates one payment from a quote POST /payments. The host-owned
// Idempotency-Key is forwarded verbatim; a changed body under the same key is the
// server's IDEMPOTENCY_CONFLICT, never a native rewrite.
func (c *Client) CreatePayment(ctx context.Context, quoteID, idempotencyKey string) (PaymentResponse, *ErrorResponse, error) {
	if !validOpaqueID(quoteID) || (c.PaymentContract == 2 && !validUUID(quoteID)) || !validIdempotencyKey(idempotencyKey) {
		return PaymentResponse{}, nil, ErrInvalidRequest
	}
	body := PaymentCreateRequest{QuoteID: quoteID}
	raw, status, err := c.requestWithQuery(ctx, http.MethodPost, "/payments", body, idempotencyKey, true, c.paymentQuery())
	if err != nil {
		return PaymentResponse{}, nil, err
	}
	if status == http.StatusOK {
		payment, err := c.decodePayment(raw)
		if err != nil {
			return PaymentResponse{}, nil, err
		}
		return payment, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return PaymentResponse{}, nil, decodeErr
	}
	// An omitted/null retryable is not an explicit final verdict. Generic error
	// decoding stays compatible; only this create response can carry the proof.
	var finality struct {
		Retryable *bool `json:"retryable"`
	}
	if json.Unmarshal(raw, &finality) == nil {
		reason, _ := envelope.Details["reason"].(string)
		envelope.expiredQuoteNoOrder = status == http.StatusConflict &&
			envelope.Code == "QUOTE_EXPIRED" && finality.Retryable != nil && !*finality.Retryable &&
			reason == "expired_quote_no_order"
	}
	if c.PaymentContract == 2 {
		if proof, ok := decodeReferralCreateNoOrder(raw, status, quoteID, idempotencyKey); ok {
			envelope.referralCreateNoOrder = &proof
		}
	}
	return PaymentResponse{}, envelope, nil
}

// GetPayment reads owner-only provider status GET /payments/{id}. It is a pure read:
// no effect, no grant, no second credit.
func (c *Client) GetPayment(ctx context.Context, paymentID string) (PaymentResponse, *ErrorResponse, error) {
	if !validPaymentPathID(paymentID) || (c.PaymentContract == 2 && !validUUID(paymentID)) {
		return PaymentResponse{}, nil, ErrInvalidRequest
	}
	raw, status, err := c.requestWithQuery(ctx, http.MethodGet, "/payments/"+paymentID, nil, "", true, c.paymentQuery())
	if err != nil {
		return PaymentResponse{}, nil, err
	}
	if status == http.StatusOK {
		payment, err := c.decodePayment(raw)
		if err != nil {
			return PaymentResponse{}, nil, err
		}
		return payment, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return PaymentResponse{}, nil, decodeErr
	}
	return PaymentResponse{}, envelope, nil
}

// CreateCheckoutSession requests the short-lived bounded hosted-checkout policy
// POST /payments/{id}/checkout-session. This is the hosted-checkout reserve: it stays
// optional and is not wired to any host action by default. The response policy is
// passed through unchanged; native never widens allowed_origins/allowed_redirects.
func (c *Client) CreateCheckoutSession(ctx context.Context, paymentID, idempotencyKey string) (CheckoutSessionResponse, *ErrorResponse, error) {
	if !validPaymentPathID(paymentID) || !validIdempotencyKey(idempotencyKey) {
		return CheckoutSessionResponse{}, nil, ErrInvalidRequest
	}
	path := "/payments/" + paymentID + "/checkout-session"
	raw, status, err := c.requestWith(ctx, http.MethodPost, path, map[string]any{}, idempotencyKey, true)
	if err != nil {
		return CheckoutSessionResponse{}, nil, err
	}
	if status == http.StatusOK {
		session, err := DecodeCheckoutSessionStrict(raw)
		if err != nil {
			return CheckoutSessionResponse{}, nil, err
		}
		return session, nil, nil
	}
	envelope, decodeErr := decodeError(raw, status)
	if decodeErr != nil {
		return CheckoutSessionResponse{}, nil, decodeErr
	}
	return CheckoutSessionResponse{}, envelope, nil
}

// ExpiredQuoteNoOrder is a bounded definitive verdict, available only on CreatePayment.
func (e *ErrorResponse) ExpiredQuoteNoOrder() bool {
	return e != nil && e.expiredQuoteNoOrder
}

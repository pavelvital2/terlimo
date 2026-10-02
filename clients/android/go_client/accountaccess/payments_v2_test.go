package accountaccess

// These fixtures are synthetic representations of client-contract-v2.safe.json,
// not endpoint captures. They verify only the defined DTO/request seams.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const syntheticV2ID = "01234567-89ab-cdef-0123-456789abcdef"
const syntheticV2Slot = "12345678-89ab-cdef-0123-456789abcdef"

func syntheticV2Product() map[string]any {
	return map[string]any{
		"kind": "subscription", "plan_id": "terlimo-30d", "device_delta": 0,
		"target_entitlement_id": syntheticV2ID, "target_valid_until": "2026-11-01T00:00:00Z",
		"valid_from": "2026-11-01T00:00:00Z", "valid_until": "2026-12-01T00:00:00Z",
		"renew_extra_slot_ids": []string{syntheticV2Slot}, "base_amount_minor": 20000,
		"extra_amount_minor": 10000, "device_limit": 3,
		"extra_slots": []any{map[string]any{"slot_id": syntheticV2Slot, "expires_at": "2026-11-01T00:00:00Z", "renew_amount_minor": 10000}},
	}
}

func syntheticV2Envelope() map[string]any {
	return map[string]any{"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2026-10-02T12:00:00Z", "schema_version": "1.0", "status": "ok"}
}

func syntheticV2Quote() map[string]any {
	value := syntheticV2Envelope()
	value["quote_id"], value["amount"], value["duration_code"], value["device_limit"] = syntheticV2ID, map[string]any{"amount_minor": 30000, "currency": "RUB"}, "days:30", 3
	value["method"], value["expires_at"], value["product"] = "sbp", "2026-10-02T12:15:00Z", syntheticV2Product()
	return value
}

func syntheticV2Plans() map[string]any {
	value := syntheticV2Envelope()
	value["plans_revision"] = "5"
	value["plans"] = []any{map[string]any{"plan_id": "terlimo-30d", "title": "30 дней", "duration_code": "days:30", "base_device_limit": 2,
		"amount": map[string]any{"amount_minor": 20000, "currency": "RUB"}, "methods": []string{"sbp", "card", "crypto"}, "product": syntheticV2Product()}}
	return value
}

func syntheticV2Payment() map[string]any {
	value := syntheticV2Envelope()
	value["payment_id"], value["payment_status"] = syntheticV2ID, "paid"
	value["checkout_reference"], value["credited_entitlement_revision"] = nil, nil
	value["access_application_state"], value["product"] = "not_requested", syntheticV2Product()
	value["credit_state"], value["credit_review_reason"], value["credited_product"] = "needs_review", "target_expired", nil
	return value
}

func syntheticV2JSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPaymentV2SyntheticStrictDTO(t *testing.T) {
	plansRaw, quoteRaw, paymentRaw := syntheticV2JSON(t, syntheticV2Plans()), syntheticV2JSON(t, syntheticV2Quote()), syntheticV2JSON(t, syntheticV2Payment())
	plans, err := DecodePlansV2Strict(plansRaw)
	if err != nil || len(plans.Plans) != 1 || plans.Plans[0].Product.ExtraSlots[0].SlotID != syntheticV2Slot {
		t.Fatalf("v2 plans: %+v %v", plans, err)
	}
	quote, err := DecodeQuoteV2Strict(quoteRaw)
	if err != nil || quote.Product == nil || quote.DeviceLimit != 3 || quote.SchemaVersion != "1.0" {
		t.Fatalf("v2 quote: %+v %v", quote, err)
	}
	payment, err := DecodePaymentV2Strict(paymentRaw)
	if err != nil || payment.PaymentStatus != "paid" || payment.CreditState != "needs_review" || payment.CreditedProduct != nil {
		t.Fatalf("v2 needs_review: %+v %v", payment, err)
	}
	if _, err := DecodePlansStrict(plansRaw); err == nil {
		t.Fatal("v1 accepted v2 plans")
	}
	if _, err := DecodeQuoteStrict(quoteRaw); err == nil {
		t.Fatal("v1 accepted v2 quote")
	}
	if _, err := DecodePaymentStrict(paymentRaw); err == nil {
		t.Fatal("v1 accepted v2 payment")
	}
	// Legacy null product and null actual receipt remain literal null.
	value := syntheticV2Payment()
	value["product"], value["credit_state"], value["credit_review_reason"] = nil, "applied", nil
	value["payment_status"], value["access_application_state"] = "refunded", "applied"
	if payment, err := DecodePaymentV2Strict(syntheticV2JSON(t, value)); err != nil || payment.Product != nil || payment.CreditedProduct != nil {
		t.Fatalf("legacy compatibility/null receipt: %+v %v", payment, err)
	}
	// Actual credit may shift the quoted period and current capacity can be greater
	// than the capacity purchased for the new period; never clamp to a commercial cap.
	value["credited_product"] = map[string]any{"valid_from": "2026-11-02T00:00:00Z", "valid_until": "2026-12-02T00:00:00Z", "device_limit": 3, "current_device_limit": 150}
	if payment, err := DecodePaymentV2Strict(syntheticV2JSON(t, value)); err != nil || payment.CreditedProduct.CurrentDeviceLimit != 150 {
		t.Fatalf("actual credit/no cap: %+v %v", payment, err)
	}
	quoteValue := syntheticV2Quote()
	quoteValue["device_limit"], quoteValue["product"] = 150, nil
	if _, err := DecodeQuoteV2Strict(syntheticV2JSON(t, quoteValue)); err != nil {
		t.Fatalf("legacy v2 quote limit capped: %v", err)
	}
	// Addon has no subscription extension or selected-renewal fabrication.
	addon := syntheticV2Product()
	addon["kind"], addon["plan_id"], addon["device_delta"] = "device_addon", "terlimo-extra-device", 1
	addon["valid_from"], addon["valid_until"] = "2026-10-02T12:00:00Z", "2026-11-01T00:00:00Z"
	addon["base_amount_minor"], addon["renew_extra_slot_ids"] = 0, []string{}
	addon["extra_slots"] = []any{map[string]any{"slot_id": syntheticV2Slot, "expires_at": "2026-11-01T00:00:00Z", "renew_amount_minor": nil}}
	quoteValue["product"], quoteValue["device_limit"], quoteValue["duration_code"] = addon, 3, "until:2026-11-01T00:00:00Z"
	if _, err := DecodeQuoteV2Strict(syntheticV2JSON(t, quoteValue)); err != nil {
		t.Fatalf("addon quote: %v", err)
	}
}

func TestPaymentV2SyntheticStrictRejections(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing product", func(v map[string]any) { delete(v, "product") }},
		{"unknown quote key", func(v map[string]any) { v["owner_account_id"] = "private" }},
		{"product unknown key", func(v map[string]any) { v["product"].(map[string]any)["owner_account_id"] = "private" }},
		{"device ID substituted", func(v map[string]any) { v["product"].(map[string]any)["renew_extra_slot_ids"] = []string{"device-1"} }},
		{"duplicate slot", func(v map[string]any) {
			v["product"].(map[string]any)["renew_extra_slot_ids"] = []string{syntheticV2Slot, syntheticV2Slot}
		}},
		{"null zero scalar", func(v map[string]any) { v["product"].(map[string]any)["device_delta"] = nil }},
		{"missing nullable key", func(v map[string]any) { delete(v["product"].(map[string]any), "target_entitlement_id") }},
		{"missing slot renewal price", func(v map[string]any) {
			delete(v["product"].(map[string]any)["extra_slots"].([]any)[0].(map[string]any), "renew_amount_minor")
		}},
		{"invalid timestamp", func(v map[string]any) { v["expires_at"] = "2026-13-02T00:00:00Z" }},
		{"product quote mismatch", func(v map[string]any) { v["device_limit"] = 4 }},
		{"zero amount", func(v map[string]any) { v["amount"].(map[string]any)["amount_minor"] = 0 }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			value := syntheticV2Quote()
			test.mutate(value)
			if _, err := DecodeQuoteV2Strict(syntheticV2JSON(t, value)); err == nil {
				t.Fatal("invalid quote accepted")
			}
		})
	}
	for _, key := range []string{"product", "credit_state", "credit_review_reason", "credited_product"} {
		value := syntheticV2Payment()
		delete(value, key)
		if _, err := DecodePaymentV2Strict(syntheticV2JSON(t, value)); err == nil {
			t.Fatalf("payment missing required %s accepted", key)
		}
	}
}

type paymentV2Doer func(*http.Request) (*http.Response, error)

func (f paymentV2Doer) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestPaymentV2SyntheticQuoteRequestBinding(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"different method", func(v map[string]any) { v["method"] = "card" }},
		{"different duration", func(v map[string]any) { v["duration_code"] = "months:3" }},
		{"different plan", func(v map[string]any) { v["product"].(map[string]any)["plan_id"] = "terlimo-3m" }},
		{"different slot", func(v map[string]any) {
			v["product"].(map[string]any)["renew_extra_slot_ids"] = []string{syntheticV2ID}
		}},
		{"null selected product", func(v map[string]any) { v["product"] = nil }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			value := syntheticV2Quote()
			test.mutate(value)
			calls := 0
			client := &Client{BaseURL: "https://synthetic.example.test/api/mobile/v1", PaymentContract: 2,
				HTTP: paymentV2Doer(func(req *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(syntheticV2JSON(t, value)))}, nil
				})}
			if _, _, err := client.CreateQuoteWithSelection(context.Background(), "terlimo-30d", "days:30", "sbp", "host-key-000000000000000001", []string{syntheticV2Slot}); err == nil {
				t.Fatal("quote for different selection accepted")
			}
			if calls != 1 {
				t.Fatalf("unexpected automatic retry: %d", calls)
			}
		})
	}
}

func TestPaymentV2SyntheticRequestsAndSelection(t *testing.T) {
	var paths, keys, bodies []string
	client := &Client{BaseURL: "https://synthetic.example.test/api/mobile/v1", PaymentContract: 2}
	client.HTTP = paymentV2Doer(func(req *http.Request) (*http.Response, error) {
		if req.URL.RawQuery != "payment_contract=2" {
			t.Fatalf("query = %q", req.URL.RawQuery)
		}
		paths, keys = append(paths, req.URL.Path), append(keys, req.Header.Get("Idempotency-Key"))
		var body []byte
		if req.Body != nil {
			body, _ = io.ReadAll(req.Body)
		}
		bodies = append(bodies, string(body))
		var response any
		switch req.URL.Path {
		case "/api/mobile/v1/plans":
			response = syntheticV2Plans()
		case "/api/mobile/v1/quotes":
			var request QuoteRequest
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			quote := syntheticV2Quote()
			quote["product"].(map[string]any)["renew_extra_slot_ids"] = request.RenewExtraSlotIDs
			quote["product"].(map[string]any)["device_limit"] = 2 + len(request.RenewExtraSlotIDs)
			quote["device_limit"] = 2 + len(request.RenewExtraSlotIDs)
			response = quote
		default:
			response = syntheticV2Payment()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(syntheticV2JSON(t, response))))}, nil
	})
	ctx := context.Background()
	if _, apiError, err := client.ListPlans(ctx); err != nil || apiError != nil {
		t.Fatalf("plans: %v %v", err, apiError)
	}
	selection := []string{syntheticV2Slot, syntheticV2ID}
	const key = "host-key-000000000000000001"
	for i := 0; i < 2; i++ {
		if _, apiError, err := client.CreateQuoteWithSelection(ctx, "terlimo-30d", "days:30", "sbp", key, selection); err != nil || apiError != nil {
			t.Fatalf("quote: %v %v", err, apiError)
		}
	}
	if keys[1] != key || keys[2] != key || bodies[1] != bodies[2] {
		t.Fatal("retry changed key or selection")
	}
	var body QuoteRequest
	if err := json.Unmarshal([]byte(bodies[1]), &body); err != nil || !reflect.DeepEqual(body.RenewExtraSlotIDs, selection) {
		t.Fatalf("selection lost: %+v %v", body, err)
	}
	if _, apiError, err := client.CreatePayment(ctx, syntheticV2ID, key); err != nil || apiError != nil {
		t.Fatalf("create: %v %v", err, apiError)
	}
	if _, apiError, err := client.GetPayment(ctx, syntheticV2ID); err != nil || apiError != nil {
		t.Fatalf("status: %v %v", err, apiError)
	}
	if len(paths) != 5 {
		t.Fatalf("unexpected requests: %v", paths)
	}
	for _, selection := range [][]string{{"device-1"}, {syntheticV2Slot, syntheticV2Slot}} {
		if _, _, err := client.CreateQuoteWithSelection(ctx, "terlimo-30d", "days:30", "sbp", key, selection); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("bad slots accepted: %v", err)
		}
	}
	if _, _, err := client.CreateQuoteWithSelection(ctx, "terlimo-extra-device", "until:2026-11-01T00:00:00Z", "sbp", key, selection); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("addon selection accepted: %v", err)
	}
	if len(paths) != 5 {
		t.Fatal("invalid requests reached wire")
	}
}

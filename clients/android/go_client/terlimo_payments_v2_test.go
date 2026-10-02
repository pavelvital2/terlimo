package main

// Synthetic v2 bridge fixtures exercise only request forwarding and the manual
// public projection. They are not real server captures or an account/menu flow.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"wg-turn-client/accountaccess"
)

type paymentV2BridgeDoer func(*http.Request) (*http.Response, error)

func (f paymentV2BridgeDoer) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestPaymentV2SyntheticBridgeSelectionAndProjection(t *testing.T) {
	const id = "01234567-89ab-cdef-0123-456789abcdef"
	const slot = "12345678-89ab-cdef-0123-456789abcdef"
	product := map[string]any{
		"kind": "subscription", "plan_id": "terlimo-30d", "device_delta": 0,
		"target_entitlement_id": id, "target_valid_until": "2026-11-01T00:00:00Z",
		"valid_from": "2026-11-01T00:00:00Z", "valid_until": "2026-12-01T00:00:00Z",
		"renew_extra_slot_ids": []string{slot}, "base_amount_minor": 20000,
		"extra_amount_minor": 10000, "device_limit": 3,
		"extra_slots": []any{map[string]any{"slot_id": slot, "expires_at": "2026-11-01T00:00:00Z", "renew_amount_minor": 10000}},
	}
	var output bytes.Buffer
	bridge := newManagedBridge(&output, "attempt", nil)
	requests := 0
	client := &accountaccess.Client{BaseURL: "https://synthetic.example.test/api/mobile/v1", PaymentContract: 2}
	client.HTTP = paymentV2BridgeDoer(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.RawQuery != "payment_contract=2" {
			t.Fatalf("v2 bridge query missing: %s", req.URL.RawQuery)
		}
		response := map[string]any{"request_id": "0123456789abcdef0123456789abcdef", "server_time": "2026-10-02T12:00:00Z", "schema_version": "1.0", "status": "ok"}
		switch req.URL.Path {
		case "/api/mobile/v1/plans":
			response["plans_revision"] = "5"
			response["plans"] = []any{map[string]any{"plan_id": "terlimo-30d", "title": "30 дней", "duration_code": "days:30", "base_device_limit": 2,
				"amount": map[string]any{"amount_minor": 20000, "currency": "RUB"}, "methods": []string{"sbp"}, "product": product}}
		case "/api/mobile/v1/quotes":
			var body accountaccess.QuoteRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil || !reflect.DeepEqual(body.RenewExtraSlotIDs, []string{slot}) || req.Header.Get("Idempotency-Key") != "host-key-000000000000000001" {
				t.Fatalf("bridge selection/key lost: %+v %v", body, err)
			}
			response["quote_id"], response["duration_code"], response["device_limit"], response["method"], response["expires_at"] = id, "days:30", 3, "sbp", "2026-10-02T12:15:00Z"
			response["amount"], response["product"] = map[string]any{"amount_minor": 30000, "currency": "RUB"}, product
		default:
			response["payment_id"], response["payment_status"], response["checkout_reference"] = id, "paid", nil
			response["credited_entitlement_revision"], response["access_application_state"], response["product"] = nil, "not_requested", product
			response["credit_state"], response["credit_review_reason"], response["credited_product"] = "needs_review", "target_expired", nil
		}
		raw, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})
	mobile := &managedMobile{client: client, bridge: bridge}
	ctx := context.Background()
	actions := []bridgeMessage{
		{"type": paymentActionPlansList},
		{"type": paymentActionQuoteCreate, "plan_id": "terlimo-30d", "duration_code": "days:30", "method": "sbp", "idempotency_key": "host-key-000000000000000001", "renew_extra_slot_ids": []any{slot}},
		{"type": paymentActionPaymentCreate, "quote_id": id, "idempotency_key": "host-key-000000000000000002"},
		{"type": paymentActionPaymentGet, "payment_id": id},
	}
	for _, action := range actions {
		output.Reset()
		mobile.handlePaymentAction(ctx, action)
		var event map[string]any
		if err := json.Unmarshal(output.Bytes(), &event); err != nil || event["state"] != "ok" || event["v"] != float64(1) || event["schema_version"] != "1.0" {
			t.Fatalf("bridge event: %s %v", output.Bytes(), err)
		}
		var projected any = event["product"]
		if action["type"] == paymentActionPlansList {
			projected = event["plans"].([]any)[0].(map[string]any)["product"]
		}
		projectedRaw, _ := json.Marshal(projected)
		productRaw, _ := json.Marshal(product)
		if !bytes.Equal(projectedRaw, productRaw) {
			t.Fatalf("public product changed: %s", projectedRaw)
		}
		if action["type"] == paymentActionPaymentCreate || action["type"] == paymentActionPaymentGet {
			if event["payment_status"] != "paid" || event["credit_state"] != "needs_review" || event["credit_review_reason"] != "target_expired" {
				t.Fatalf("paid review flattened: %+v", event)
			}
			if value, present := event["credited_product"]; !present || value != nil {
				t.Fatalf("nullable actual snapshot lost: %+v", event)
			}
		}
	}
	before := requests
	for _, invalid := range []any{nil, "device-id", []any{slot, 7}, []any{"device-id"}, []any{slot, slot}} {
		output.Reset()
		action := bridgeMessage{"type": paymentActionQuoteCreate, "plan_id": "terlimo-30d", "duration_code": "days:30", "method": "sbp", "idempotency_key": "host-key-000000000000000001", "renew_extra_slot_ids": invalid}
		mobile.handleQuoteCreate(ctx, action)
		if !strings.Contains(output.String(), `"code":"INVALID_REQUEST"`) {
			t.Fatalf("bad selection not rejected: %s", output.String())
		}
	}
	if requests != before {
		t.Fatal("invalid selection reached transport")
	}
}

func TestPaymentV2SyntheticBridgeNullAndActualCredit(t *testing.T) {
	legacy := paymentResultMessage(paymentEventPaymentGet, accountaccess.PaymentResponse{})
	for _, key := range []string{"product", "credit_state", "credit_review_reason", "credited_product"} {
		if _, present := legacy[key]; present {
			t.Fatalf("v1 gained %s", key)
		}
	}
	value := accountaccess.PaymentResponse{PaymentContract: 2, CreditState: "applied"}
	event := paymentResultMessage(paymentEventPaymentGet, value)
	if product, present := event["product"]; !present || product != nil {
		t.Fatal("v2 product null not projected")
	}
	if credited, present := event["credited_product"]; !present || credited != nil {
		t.Fatal("legacy actual snapshot fabricated")
	}
	until := "2026-12-02T00:00:00Z"
	value.CreditedProduct = &accountaccess.CreditedProduct{ValidFrom: "2026-11-02T00:00:00Z", ValidUntil: &until, DeviceLimit: 3, CurrentDeviceLimit: 150}
	event = paymentResultMessage(paymentEventPaymentGet, value)
	actual := event["credited_product"].(bridgeMessage)
	if len(actual) != 4 || actual["valid_from"] != value.CreditedProduct.ValidFrom || actual["current_device_limit"] != 150 {
		t.Fatalf("actual credit projection lost: %+v", actual)
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"wg-turn-client/accountaccess"
)

// One cross-language path over captured server envelopes. This exercises the real
// handlers/manual projection, not another full sweep of the fourteen DTO fixtures.
func TestReviewedPaymentBridgeCapture(t *testing.T) {
	raw, err := os.ReadFile("testdata/payment-wire-v2-reviewed.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	cases := [][3]string{
		{"subscription", "quote_envelope", paymentActionQuoteCreate},
		{"addon", "quote_envelope", paymentActionQuoteCreate},
		{"selected_renewal", "plans_envelope", paymentActionPlansList},
		{"selected_renewal", "quote_envelope", paymentActionQuoteCreate},
		{"selected_renewal", "paid_envelope", paymentActionPaymentGet},
		{"paid_needs_review", "status_envelope", paymentActionPaymentGet},
	}
	var captures []map[string]any
	for _, item := range cases {
		var scenario map[string]json.RawMessage
		if err := json.Unmarshal(fixtures[item[0]], &scenario); err != nil {
			t.Fatal(err)
		}
		envelope := scenario[item[1]]
		var source map[string]any
		if err := json.Unmarshal(envelope, &source); err != nil {
			t.Fatal(err)
		}
		action := bridgeMessage{"type": item[2]}
		switch item[2] {
		case paymentActionQuoteCreate:
			product := source["product"].(map[string]any)
			action["plan_id"] = product["plan_id"]
			action["duration_code"], action["method"] = source["duration_code"], source["method"]
			action["renew_extra_slot_ids"] = product["renew_extra_slot_ids"]
			action["idempotency_key"] = "reviewed-fixture-host-key-0001"
		case paymentActionPaymentGet:
			action["payment_id"] = source["payment_id"]
		}
		var output bytes.Buffer
		client := &accountaccess.Client{BaseURL: "https://captured.invalid/api/mobile/v1", PaymentContract: 2}
		client.HTTP = paymentV2BridgeDoer(func(req *http.Request) (*http.Response, error) {
			if req.URL.RawQuery != "payment_contract=2" {
				t.Fatal("query negotiation lost")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(envelope))}, nil
		})
		mobile := &managedMobile{client: client, bridge: newManagedBridge(&output, "reviewed-wire", nil)}
		mobile.handlePaymentAction(context.Background(), action)
		var event map[string]any
		if err := json.Unmarshal(output.Bytes(), &event); err != nil || event["state"] != "ok" {
			t.Fatalf("%s/%s: bridge rejected: %s (%v)", item[0], item[1], output.Bytes(), err)
		}
		captures = append(captures, map[string]any{"scenario": item[0], "envelope": item[1], "event": event})
	}
	encoded, err := json.MarshalIndent(captures, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Explicit capture path writes only a safe test artifact for the Kotlin parser.
	if path := os.Getenv("TERLIMO_REVIEWED_BRIDGE_CAPTURE"); path != "" {
		if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("actual server envelopes -> production bridge: %d cases", len(captures))
}

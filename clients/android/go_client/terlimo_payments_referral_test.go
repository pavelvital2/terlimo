package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"testing"

	"wg-turn-client/accountaccess"
)

func TestReferralPaymentBridgeSharedFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/TERLIMO_IMPLEMENTATION/referral_20261003/payment-client-wire-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases map[string]struct {
			Server     json.RawMessage
			Native     map[string]any
			HTTPStatus int `json:"http_status"`
		}
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	for name, fixture := range file.Cases {
		t.Run(name, func(t *testing.T) {
			var server map[string]any
			if err := json.Unmarshal(fixture.Server, &server); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			client := &accountaccess.Client{BaseURL: "https://example.test/api/mobile/v1", PaymentContract: 2}
			calls := 0
			client.HTTP = paymentV2BridgeDoer(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.RawQuery != "payment_contract=2" {
					t.Fatal("contract query lost")
				}
				status := fixture.HTTPStatus
				if status == 0 {
					status = 200
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(fixture.Server))}, nil
			})
			mobile := &managedMobile{client: client, bridge: newManagedBridge(&output, "referral-payment-attempt", nil)}
			action := bridgeMessage{"idempotency_key": "original-create-key-0001"}
			switch fixture.Native["type"] {
			case paymentEventQuoteCreate:
				action["type"] = paymentActionQuoteCreate
				action["plan_id"] = server["product"].(map[string]any)["plan_id"]
				action["duration_code"], action["method"] = server["duration_code"], server["method"]
				action["renew_extra_slot_ids"] = server["product"].(map[string]any)["renew_extra_slot_ids"]
			case paymentEventPaymentCreate:
				action["type"], action["quote_id"] = paymentActionPaymentCreate, "fe6b69a7-82b9-4d01-80cd-d60e42c21b65"
			case paymentEventPaymentGet:
				action["type"], action["payment_id"] = paymentActionPaymentGet, server["payment_id"]
			default:
				t.Fatalf("unhandled fixture event %v", fixture.Native["type"])
			}
			mobile.handlePaymentAction(context.Background(), action)
			var actual map[string]any
			if err := json.Unmarshal(output.Bytes(), &actual); err != nil {
				t.Fatalf("event decode %v: %s", err, output.String())
			}
			if !reflect.DeepEqual(actual, fixture.Native) {
				t.Fatalf("exact native fixture mismatch\nactual=%s\nwant=%v", output.String(), fixture.Native)
			}
			if calls != 1 {
				t.Fatalf("unexpected retry: %d", calls)
			}
			if fixture.HTTPStatus != 0 {
				output.Reset()
				action["idempotency_key"] = "different-original-key-0001"
				mobile.handlePaymentAction(context.Background(), action)
				if err := json.Unmarshal(output.Bytes(), &actual); err != nil {
					t.Fatal(err)
				}
				// Decode into a fresh map: json.Unmarshal retains absent keys otherwise.
				actual = nil
				_ = json.Unmarshal(output.Bytes(), &actual)
				for _, key := range []string{"create_resolution", "reason", "http_status", "retryable", "request_id"} {
					if _, present := actual[key]; present {
						t.Fatalf("mismatched K exposed %s: %v", key, actual)
					}
				}
			}
		})
	}
}

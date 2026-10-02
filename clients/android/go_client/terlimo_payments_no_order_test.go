package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExpiredNoOrderBridgeBoundary(t *testing.T) {
	for _, reason := range []string{"expired_quote_no_order", "", "unknown_reason"} {
		t.Run("reason_"+reason, func(t *testing.T) {
			raw := paymentBridgeFixture(t, "error_service_unavailable.json", paymentBridgeErrorFixtureSHA)
			var envelope map[string]any
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			envelope["code"] = "QUOTE_EXPIRED"
			envelope["retryable"] = false
			envelope["details"] = map[string]any{"reason": reason, "private": "must not reach bridge"}
			raw, _ = json.Marshal(envelope)
			h := newPaymentHarness(t, func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasPrefix(r.URL.Path, "/api/mobile/v1/payments") && r.URL.Path != "/api/mobile/v1/quotes" && r.URL.Path != "/api/mobile/v1/plans" {
					return false
				}
				w.WriteHeader(409)
				_, _ = w.Write(raw)
				return true
			})
			h.waitForSubstring(`"type":"account_access"`, 5*time.Second)
			h.sendAction(bridgeMessage{"type": paymentActionPaymentCreate, "quote_id": "q-1", "idempotency_key": "test-key-0000000000000001"})
			msg := h.waitForEvent(paymentEventPaymentCreate, 5*time.Second)
			_, has := msg["reason"]
			if msg.string("code") != "QUOTE_EXPIRED" || has != (reason == "expired_quote_no_order") || (has && msg.string("reason") != reason) {
				t.Fatalf("wrong projection: %+v", msg)
			}
			if len(msg) != 5+map[bool]int{false: 0, true: 1}[has] {
				t.Fatalf("unbounded fields: %+v", msg)
			}
			for _, action := range []bridgeMessage{
				{"type": paymentActionPaymentGet, "payment_id": "pay-1"},
				{"type": paymentActionQuoteCreate, "plan_id": "p30", "duration_code": "days:30", "method": "card", "idempotency_key": "test-key-0000000000000002"},
				{"type": paymentActionPlansList},
			} {
				h.sendAction(action)
				event := map[string]string{paymentActionPaymentGet: paymentEventPaymentGet, paymentActionQuoteCreate: paymentEventQuoteCreate, paymentActionPlansList: paymentEventPlansList}[action.string("type")]
				msg = h.waitForEvent(event, 5*time.Second)
				if _, ok := msg["reason"]; ok {
					t.Fatalf("non-create leaked reason: %+v", msg)
				}
			}
			reqs := h.requestsFor("/api/mobile/v1/payments")
			if len(reqs) != 1 || reqs[0].Key != "test-key-0000000000000001" || !strings.Contains(reqs[0].Body, `"quote_id":"q-1"`) {
				t.Fatal("Q/K changed or retried")
			}
		})
	}
}

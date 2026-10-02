package accountaccess

import (
	"context"
	"net/http"
	"testing"
)

func TestExpiredNoOrderCreateBoundary(t *testing.T) {
	base := paymentsFixture(t, "error_service_unavailable.json", paymentsErrorFixtureSHA)
	for _, tc := range []struct {
		name   string
		status int
		code   string
		reason any
		retry  any
		want   bool
	}{
		{"exact", 409, "QUOTE_EXPIRED", "expired_quote_no_order", false, true},
		{"plain", 409, "QUOTE_EXPIRED", nil, false, false},
		{"unknown", 409, "QUOTE_EXPIRED", "future_reason", false, false},
		{"malformed", 409, "QUOTE_EXPIRED", 17, false, false},
		{"object", 409, "QUOTE_EXPIRED", map[string]any{"reason": "expired_quote_no_order"}, false, false},
		{"retryable", 409, "QUOTE_EXPIRED", "expired_quote_no_order", true, false},
		{"missing_retryable", 409, "QUOTE_EXPIRED", "expired_quote_no_order", "missing", false},
		{"null_retryable", 409, "QUOTE_EXPIRED", "expired_quote_no_order", nil, false},
		{"wrong_http", 503, "QUOTE_EXPIRED", "expired_quote_no_order", false, false},
		{"unavailable", 503, "SERVICE_UNAVAILABLE", "expired_quote_no_order", false, false},
		{"not_found", 409, "NOT_FOUND", "expired_quote_no_order", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := mutateJSON(t, base, func(v map[string]any) {
				v["code"] = tc.code
				v["retryable"] = tc.retry
				if tc.retry == "missing" {
					delete(v, "retryable")
				}
				v["details"] = map[string]any{"reason": tc.reason, "unknown_detail": "not projected"}
			})
			capture := &paymentCapture{respond: func(w http.ResponseWriter, r *http.Request) bool {
				w.WriteHeader(tc.status)
				_, _ = w.Write(raw)
				return true
			}}
			client, _ := paymentTestClient(t, capture)
			client.PaymentContract = 2
			_, e, err := client.CreatePayment(context.Background(), "11111111-1111-4111-8111-111111111111", "test-key-0000000000000001")
			if err != nil || e == nil || e.ExpiredQuoteNoOrder() != tc.want {
				t.Fatalf("unexpected classification: %+v %v", e, err)
			}
			if e.Details["unknown_detail"] != "not projected" {
				t.Fatal("generic details were lost")
			}
			if tc.want {
				_, e, err = client.GetPayment(context.Background(), "22222222-2222-4222-8222-222222222222")
				if err != nil || e == nil || e.ExpiredQuoteNoOrder() {
					t.Fatal("get must not authorize no-create")
				}
				_, e, err = client.ListPlans(context.Background())
				if err != nil || e == nil || e.ExpiredQuoteNoOrder() {
					t.Fatal("plans must not authorize no-create")
				}
			}
		})
	}
}

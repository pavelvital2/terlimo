package accountaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func referralPaymentFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../../docs/TERLIMO_IMPLEMENTATION/referral_20261003/payment-client-wire-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases map[string]struct{ Server json.RawMessage }
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases[name].Server) == 0 {
		t.Fatalf("missing case %s", name)
	}
	return file.Cases[name].Server
}

func TestReferralPaymentSharedFixtures(t *testing.T) {
	for _, name := range []string{"discounted_quote", "discounted_main_quote", "legacy_10", "legacy_20"} {
		t.Run(name, func(t *testing.T) {
			quote, err := DecodeQuoteV2Strict(referralPaymentFixture(t, name))
			if err != nil {
				t.Fatal(err)
			}
			if (quote.Pricing != nil) != strings.HasPrefix(name, "discounted_") {
				t.Fatal("pricing presence changed")
			}
		})
	}
	for _, name := range []string{"discounted_create", "discounted_reconciling", "discounted_credited", "legacy_addon"} {
		t.Run(name, func(t *testing.T) {
			payment, err := DecodePaymentV2Strict(referralPaymentFixture(t, name))
			if err != nil {
				t.Fatal(err)
			}
			if (payment.Pricing != nil) != strings.HasPrefix(name, "discounted_") {
				t.Fatal("pricing presence changed")
			}
			if name == "discounted_credited" && (payment.CreditedProduct == nil || payment.CreditedProduct.Pricing == nil) {
				t.Fatal("receipt pricing lost")
			}
		})
	}
}

func TestReferralPaymentStrictPricing(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"null", func(v map[string]any) { v["pricing"] = nil }},
		{"missing scalar", func(v map[string]any) { delete(v["pricing"].(map[string]any), "terms_version") }},
		{"null scalar", func(v map[string]any) { v["pricing"].(map[string]any)["discount_minor"] = nil }},
		{"fraction", func(v map[string]any) { v["pricing"].(map[string]any)["discount_minor"] = 10000.5 }},
		{"unknown", func(v map[string]any) { v["pricing"].(map[string]any)["eligible"] = true }},
		{"terms", func(v map[string]any) { v["pricing"].(map[string]any)["terms_version"] = "future" }},
		{"discount", func(v map[string]any) { v["pricing"].(map[string]any)["discount_minor"] = 5000 }},
		{"payable", func(v map[string]any) { v["pricing"].(map[string]any)["payable_amount_minor"] = 30167 }},
		{"amount", func(v map[string]any) { v["amount"].(map[string]any)["amount_minor"] = 30167 }},
		{"product null", func(v map[string]any) { v["product"] = nil }},
		{"extra absent", func(v map[string]any) { v["product"].(map[string]any)["extra_slots"] = []any{} }},
		{"extra not fully paid", func(v map[string]any) {
			p := v["product"].(map[string]any)
			p["base_amount_minor"], p["extra_amount_minor"] = 20167, 10000
		}},
		{"selected renewal null", func(v map[string]any) {
			p := v["product"].(map[string]any)
			for _, slot := range p["extra_slots"].([]any) {
				slot.(map[string]any)["renew_amount_minor"] = nil
			}
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal(referralPaymentFixture(t, "discounted_quote"), &value); err != nil {
				t.Fatal(err)
			}
			test.mutate(value)
			if _, err := DecodeQuoteV2Strict(syntheticV2JSON(t, value)); err == nil {
				t.Fatal("invalid pricing accepted")
			}
		})
	}
	for _, raw := range []string{
		strings.Replace(string(referralPaymentFixture(t, "discounted_quote")), `"discount_minor": 10000`, `"discount_minor": 10000,"discount_minor": 10000`, 1),
		strings.Replace(string(referralPaymentFixture(t, "discounted_quote")), `"base_amount_minor": 30167`, `"base_amount_minor": 9223372036854775808`, 1),
	} {
		if _, err := DecodeQuoteV2Strict([]byte(raw)); err == nil {
			t.Fatal("duplicate/overflow accepted")
		}
	}
	for _, field := range []string{"referral_discount_state", "credited_product"} {
		var value map[string]any
		_ = json.Unmarshal(referralPaymentFixture(t, "discounted_credited"), &value)
		if field == "credited_product" {
			delete(value[field].(map[string]any), "pricing")
		} else {
			value[field] = "released"
		}
		if _, err := DecodePaymentV2Strict(syntheticV2JSON(t, value)); err == nil {
			t.Fatalf("invalid %s accepted", field)
		}
	}
	for _, state := range []any{nil, "reserved"} {
		var value map[string]any
		_ = json.Unmarshal(referralPaymentFixture(t, "legacy_addon"), &value)
		value["referral_discount_state"] = state
		if _, err := DecodePaymentV2Strict(syntheticV2JSON(t, value)); err == nil {
			t.Fatal("state without pricing accepted")
		}
	}
}

func TestReferralPaymentNoOrderCreateBoundary(t *testing.T) {
	const quote = "fe6b69a7-82b9-4d01-80cd-d60e42c21b65"
	const key = "original-create-key-0001"
	for _, name := range []string{"no_order_reserved", "no_order_quote_changed"} {
		t.Run(name, func(t *testing.T) {
			raw := referralPaymentFixture(t, name)
			calls := 0
			client := &Client{BaseURL: "https://example.test/api/mobile/v1", PaymentContract: 2,
				HTTP: paymentV2Doer(func(req *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: 409, Body: io.NopCloser(bytes.NewReader(raw))}, nil
				})}
			_, apiError, err := client.CreatePayment(context.Background(), quote, key)
			if err != nil || apiError == nil {
				t.Fatalf("create failed: %v %+v", err, apiError)
			}
			proof, ok := apiError.ReferralCreateNoOrder()
			if !ok || proof.QuoteID != quote || proof.RequestIdempotencyKey != key || calls != 1 {
				t.Fatal("correlated proof absent or automatic retry")
			}
			_, apiError, _ = client.GetPayment(context.Background(), quote)
			if _, ok := apiError.ReferralCreateNoOrder(); ok {
				t.Fatal("GET minted proof")
			}
			_, apiError, _ = client.CreatePayment(context.Background(), quote, "another-create-key-0001")
			if _, ok := apiError.ReferralCreateNoOrder(); ok {
				t.Fatal("different K minted proof")
			}
			_, apiError, _ = client.CreatePayment(context.Background(), syntheticV2ID, key)
			if _, ok := apiError.ReferralCreateNoOrder(); ok {
				t.Fatal("different Q minted proof")
			}
			decoded, _ := DecodeErrorStrict(raw)
			if _, ok := decoded.ReferralCreateNoOrder(); ok {
				t.Fatal("generic decoder minted proof")
			}
		})
	}
	mutations := []func(map[string]any){
		func(v map[string]any) { v["retryable"] = true },
		func(v map[string]any) { delete(v, "retryable") },
		func(v map[string]any) { v["request_id"] = "invalid" },
		func(v map[string]any) { v["code"] = "QUOTE_EXPIRED" },
		func(v map[string]any) { v["details"].(map[string]any)["unknown"] = true },
		func(v map[string]any) { v["details"].(map[string]any)["reason"] = "referral_quote_changed" },
		func(v map[string]any) {
			v["details"].(map[string]any)["create_resolution"].(map[string]any)["quote_id"] = nil
		},
	}
	for i, mutate := range mutations {
		var value map[string]any
		_ = json.Unmarshal(referralPaymentFixture(t, "no_order_reserved"), &value)
		mutate(value)
		if _, ok := decodeReferralCreateNoOrder(syntheticV2JSON(t, value), 409, quote, key); ok {
			t.Fatalf("mutation %d minted proof", i)
		}
	}
	if _, ok := decodeReferralCreateNoOrder(referralPaymentFixture(t, "no_order_reserved"), 502, quote, key); ok {
		t.Fatal("502 minted proof")
	}
}

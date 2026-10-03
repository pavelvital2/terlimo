package accountaccess

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"

	"wg-turn-client/wlwire"
)

// PaymentPricing is the frozen server price, including fully paid selected extras.
// Product.BaseAmountMinor remains the undiscounted MAIN component.
type PaymentPricing struct {
	BaseAmountMinor    int64  `json:"base_amount_minor"`
	DiscountMinor      int64  `json:"discount_minor"`
	PayableAmountMinor int64  `json:"payable_amount_minor"`
	Currency           string `json:"currency"`
	DiscountKind       string `json:"discount_kind"`
	TermsVersion       string `json:"terms_version"`
}

func (pricing *PaymentPricing) UnmarshalJSON(raw []byte) error {
	type plain PaymentPricing
	var value plain
	if err := wlwire.StrictJSON(raw, &value); err != nil {
		return fmt.Errorf("referral pricing invalid")
	}
	if err := requiredNonNull(raw, "base_amount_minor", "discount_minor", "payable_amount_minor", "currency", "discount_kind", "terms_version"); err != nil {
		return err
	}
	if value.DiscountMinor != 10000 || value.BaseAmountMinor <= value.DiscountMinor ||
		value.PayableAmountMinor <= 0 || value.BaseAmountMinor-value.DiscountMinor != value.PayableAmountMinor ||
		value.Currency != "RUB" || value.DiscountKind != "referral_first_main" || value.TermsVersion != ReferralTermsVersion {
		return fmt.Errorf("referral pricing invalid")
	}
	*pricing = PaymentPricing(value)
	return nil
}

func optionalPaymentFieldNonNull(raw []byte, keys ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range keys {
		if value, present := fields[key]; present && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("optional payment field %q is null", key)
		}
	}
	return nil
}

func pricingMatchesProduct(pricing *PaymentPricing, product *Product) bool {
	if !(pricing != nil && product != nil && product.Kind == "subscription" && product.BaseAmountMinor > 10000 &&
		product.ExtraAmountMinor >= 0 && product.BaseAmountMinor <= math.MaxInt64-product.ExtraAmountMinor &&
		product.BaseAmountMinor+product.ExtraAmountMinor == pricing.BaseAmountMinor) {
		return false
	}
	selected := make(map[string]bool, len(product.RenewExtraSlotIDs))
	for _, id := range product.RenewExtraSlotIDs {
		selected[strings.ToLower(id)] = true
	}
	var extraSum int64
	for _, slot := range product.ExtraSlots {
		id := strings.ToLower(slot.SlotID)
		if !selected[id] {
			continue
		}
		if slot.RenewAmountMinor == nil || *slot.RenewAmountMinor <= 0 || extraSum > math.MaxInt64-*slot.RenewAmountMinor {
			return false
		}
		extraSum += *slot.RenewAmountMinor
		delete(selected, id)
	}
	return len(selected) == 0 && extraSum == product.ExtraAmountMinor
}

// PaymentCreateResolution is a definitive no-order verdict for exactly one Q/K.
// It never permits release of an existing invoice/reservation or a GET-side clear.
type PaymentCreateResolution struct {
	Kind                  string `json:"kind"`
	QuoteID               string `json:"quote_id"`
	RequestIdempotencyKey string `json:"request_idempotency_key"`
	Reason                string `json:"reason"`
}

func decodeReferralCreateNoOrder(raw []byte, status int, quoteID, key string) (PaymentCreateResolution, bool) {
	var envelope ErrorResponse
	var fields map[string]json.RawMessage
	if status != http.StatusConflict || wlwire.StrictJSON(raw, &envelope) != nil ||
		json.Unmarshal(raw, &fields) != nil || requiredNonNull(raw, "request_id", "server_time", "schema_version", "status", "code", "retryable", "details") != nil ||
		!requestIDPattern.MatchString(envelope.RequestID) || !ValidUtcTime(envelope.ServerTime) || envelope.SchemaVersion != SchemaVersion ||
		envelope.Status != "error" || envelope.Retryable {
		return PaymentCreateResolution{}, false
	}
	var details struct {
		Reason     string                  `json:"reason"`
		Resolution PaymentCreateResolution `json:"create_resolution"`
	}
	if wlwire.StrictJSON(fields["details"], &details) != nil || requiredNonNull(fields["details"], "reason", "create_resolution") != nil {
		return PaymentCreateResolution{}, false
	}
	var rawDetails map[string]json.RawMessage
	_ = json.Unmarshal(fields["details"], &rawDetails)
	if requiredNonNull(rawDetails["create_resolution"], "kind", "quote_id", "request_idempotency_key", "reason") != nil {
		return PaymentCreateResolution{}, false
	}
	proof := details.Resolution
	if proof.Kind != "no_order" || !validUUID(proof.QuoteID) || !validIdempotencyKey(proof.RequestIdempotencyKey) ||
		proof.QuoteID != quoteID || proof.RequestIdempotencyKey != key || details.Reason != proof.Reason {
		return PaymentCreateResolution{}, false
	}
	if !((envelope.Code == "REFERRAL_DISCOUNT_RESERVED" && proof.Reason == "referral_discount_reserved") ||
		(envelope.Code == "QUOTE_EXPIRED" && proof.Reason == "referral_quote_changed")) {
		return PaymentCreateResolution{}, false
	}
	return proof, true
}

// ReferralCreateNoOrder is set only by the correlated CreatePayment response.
// Decoders, GET, quote reads and arbitrary ErrorResponse values cannot mint it.
func (errorResponse *ErrorResponse) ReferralCreateNoOrder() (PaymentCreateResolution, bool) {
	if errorResponse == nil || errorResponse.referralCreateNoOrder == nil {
		return PaymentCreateResolution{}, false
	}
	return *errorResponse.referralCreateNoOrder, true
}

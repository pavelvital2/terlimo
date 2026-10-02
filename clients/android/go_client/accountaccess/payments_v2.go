package accountaccess

// Payment contract v2 is negotiated per payment route. The envelope stays 1.0.
// Its wire shapes are separate from the strict v1 decoders; no additional fields
// are accepted by a default Client or by Decode*Strict.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var paymentUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func validUUID(value string) bool { return paymentUUIDPattern.MatchString(value) }

func validSlotIDs(ids []string) bool {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		canonical := strings.ToLower(id)
		if !validUUID(id) || seen[canonical] {
			return false
		}
		seen[canonical] = true
	}
	return true
}

func validProductPlanID(value string) bool {
	switch value {
	case "terlimo-30d", "terlimo-3m", "terlimo-6m", "terlimo-extra-device":
		return true
	}
	return false
}

func validPaymentUTC(value string) bool {
	_, err := parseUtc(value)
	return err == nil
}

func validDurationCodeV2(value string) bool {
	return validDurationCode(value) || (strings.HasPrefix(value, "until:") && validPaymentUTC(strings.TrimPrefix(value, "until:")))
}

func quoteMatchesV2Request(quote QuoteResponse, request QuoteRequest) bool {
	if quote.DurationCode != request.DurationCode || quote.Method != request.Method {
		return false
	}
	if quote.Product == nil {
		return request.PlanID != "terlimo-extra-device" && len(request.RenewExtraSlotIDs) == 0
	}
	if quote.Product.PlanID != request.PlanID || len(quote.Product.RenewExtraSlotIDs) != len(request.RenewExtraSlotIDs) {
		return false
	}
	selected := make(map[string]bool, len(request.RenewExtraSlotIDs))
	for _, id := range request.RenewExtraSlotIDs {
		selected[strings.ToLower(id)] = true
	}
	for _, id := range quote.Product.RenewExtraSlotIDs {
		if !selected[strings.ToLower(id)] {
			return false
		}
	}
	return true
}

// requiredNonNull supplements strict required-key decoding for zero-valued scalars.
func requiredNonNull(raw []byte, keys ...string) error {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	for _, key := range keys {
		value, present := values[key]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("required non-null field %q missing", key)
		}
	}
	return nil
}

// ExtraSlot is an independently paid place, identified by slot_id, not device_id.
type ExtraSlot struct {
	SlotID           string `json:"slot_id"`
	ExpiresAt        string `json:"expires_at"`
	RenewAmountMinor *int64 `json:"renew_amount_minor"`
}

func (s *ExtraSlot) UnmarshalJSON(raw []byte) error {
	type plain ExtraSlot
	var value plain
	if err := unmarshalStrictRequired(raw, &value, "slot_id", "expires_at", "renew_amount_minor"); err != nil {
		return err
	}
	if !validUUID(value.SlotID) || !validPaymentUTC(value.ExpiresAt) || (value.RenewAmountMinor != nil && *value.RenewAmountMinor <= 0) {
		return fmt.Errorf("extra slot invalid")
	}
	*s = ExtraSlot(value)
	return nil
}

// Product is the immutable server offer. ExtraSlots is kept verbatim: plans expose
// renewal choices, while quote membership is defined by the server snapshot.
type Product struct {
	Kind                string      `json:"kind"`
	PlanID              string      `json:"plan_id"`
	DeviceDelta         int         `json:"device_delta"`
	TargetEntitlementID *string     `json:"target_entitlement_id"`
	TargetValidUntil    *string     `json:"target_valid_until"`
	ValidFrom           string      `json:"valid_from"`
	ValidUntil          string      `json:"valid_until"`
	RenewExtraSlotIDs   []string    `json:"renew_extra_slot_ids"`
	BaseAmountMinor     int64       `json:"base_amount_minor"`
	ExtraAmountMinor    int64       `json:"extra_amount_minor"`
	DeviceLimit         int         `json:"device_limit"`
	ExtraSlots          []ExtraSlot `json:"extra_slots"`
}

func (p *Product) UnmarshalJSON(raw []byte) error {
	type plain Product
	var value plain
	if err := unmarshalStrictRequired(raw, &value, "kind", "plan_id", "device_delta", "target_entitlement_id", "target_valid_until",
		"valid_from", "valid_until", "renew_extra_slot_ids", "base_amount_minor", "extra_amount_minor", "device_limit", "extra_slots"); err != nil {
		return err
	}
	if err := requiredNonNull(raw, "device_delta", "base_amount_minor", "extra_amount_minor"); err != nil {
		return err
	}
	if !validProductPlanID(value.PlanID) || !validPaymentUTC(value.ValidFrom) || !validPaymentUTC(value.ValidUntil) ||
		value.DeviceLimit < 2 || value.BaseAmountMinor < 0 || value.ExtraAmountMinor < 0 || value.RenewExtraSlotIDs == nil ||
		value.ExtraSlots == nil || !validSlotIDs(value.RenewExtraSlotIDs) {
		return fmt.Errorf("product invalid")
	}
	if value.TargetEntitlementID != nil && !validUUID(*value.TargetEntitlementID) {
		return fmt.Errorf("target_entitlement_id invalid")
	}
	if value.TargetValidUntil != nil && !validPaymentUTC(*value.TargetValidUntil) {
		return fmt.Errorf("target_valid_until invalid")
	}
	start, _ := time.Parse(time.RFC3339Nano, value.ValidFrom)
	end, _ := time.Parse(time.RFC3339Nano, value.ValidUntil)
	if !end.After(start) {
		return fmt.Errorf("product period invalid")
	}
	switch value.Kind {
	case "subscription":
		if value.PlanID == "terlimo-extra-device" || value.DeviceDelta != 0 || value.DeviceLimit-2 != len(value.RenewExtraSlotIDs) {
			return fmt.Errorf("subscription product invalid")
		}
	case "device_addon":
		if value.PlanID != "terlimo-extra-device" || value.DeviceDelta != 1 || value.BaseAmountMinor != 0 ||
			value.TargetEntitlementID == nil || value.TargetValidUntil == nil || value.ValidUntil != *value.TargetValidUntil || len(value.RenewExtraSlotIDs) != 0 {
			return fmt.Errorf("addon product invalid")
		}
	default:
		return fmt.Errorf("product kind invalid")
	}
	seen := make(map[string]bool, len(value.ExtraSlots))
	for _, slot := range value.ExtraSlots {
		id := strings.ToLower(slot.SlotID)
		if seen[id] {
			return fmt.Errorf("product extra_slots invalid")
		}
		seen[id] = true
	}
	*p = Product(value)
	return nil
}

// CreditedProduct is the actual credited period, distinct from the quoted Product.
// A legacy payment may have no such snapshot; the client never synthesizes one.
type CreditedProduct struct {
	ValidFrom          string  `json:"valid_from"`
	ValidUntil         *string `json:"valid_until"`
	DeviceLimit        int     `json:"device_limit"`
	CurrentDeviceLimit int     `json:"current_device_limit"`
}

func (p *CreditedProduct) UnmarshalJSON(raw []byte) error {
	type plain CreditedProduct
	var value plain
	if err := unmarshalStrictRequired(raw, &value, "valid_from", "valid_until", "device_limit", "current_device_limit"); err != nil {
		return err
	}
	if !validPaymentUTC(value.ValidFrom) || (value.ValidUntil != nil && !validPaymentUTC(*value.ValidUntil)) || value.DeviceLimit < 2 || value.CurrentDeviceLimit < 2 {
		return fmt.Errorf("credited product invalid")
	}
	*p = CreditedProduct(value)
	return nil
}

// Alias embeddings have no legacy UnmarshalJSON methods. Their explicit v2 fields
// form a closed key set, while the shared public DTOs keep v1 JSON unchanged.
type planV1Fields Plan
type planV2Wire struct {
	planV1Fields
	Product *Product `json:"product"`
}

func (p *planV2Wire) UnmarshalJSON(raw []byte) error {
	type plain planV2Wire
	var value plain
	if err := unmarshalStrictRequired(raw, &value, "plan_id", "title", "duration_code", "base_device_limit", "amount", "methods", "product"); err != nil {
		return err
	}
	if err := requiredNonNull(raw, "title"); err != nil {
		return err
	}
	*p = planV2Wire(value)
	return nil
}

type plansV1Fields PlansResponse
type plansV2Wire struct {
	plansV1Fields
	Plans []planV2Wire `json:"plans"`
}

func DecodePlansV2Strict(raw []byte) (PlansResponse, error) {
	var wire plansV2Wire
	if err := unmarshalStrictRequired(raw, &wire, "request_id", "server_time", "schema_version", "status", "plans", "plans_revision"); err != nil {
		return PlansResponse{}, fmt.Errorf("plans v2 decode: %w", err)
	}
	response := PlansResponse(wire.plansV1Fields)
	if !validServerEnvelope(response.RequestID, response.ServerTime, response.SchemaVersion, response.Status) || !ValidRevision(response.PlansRevision) || wire.Plans == nil {
		return PlansResponse{}, fmt.Errorf("plans v2 envelope invalid")
	}
	response.Plans = make([]Plan, 0, len(wire.Plans))
	for _, value := range wire.Plans {
		plan := Plan(value.planV1Fields)
		plan.Product, plan.PaymentContract = value.Product, 2
		if !validProductPlanID(plan.PlanID) || len(plan.Title) > 128 || !validDurationCodeV2(plan.DurationCode) || plan.BaseDeviceLimit != 2 ||
			plan.Amount.AmountMinor <= 0 || plan.Amount.Currency != "RUB" || plan.Methods == nil {
			return PlansResponse{}, fmt.Errorf("plan v2 invalid")
		}
		seen := map[string]bool{}
		for _, method := range plan.Methods {
			if !validPaymentMethod(method) || seen[method] {
				return PlansResponse{}, fmt.Errorf("plan v2 methods invalid")
			}
			seen[method] = true
		}
		if plan.Product != nil && plan.PlanID != plan.Product.PlanID {
			return PlansResponse{}, fmt.Errorf("plan v2 product mismatch")
		}
		response.Plans = append(response.Plans, plan)
	}
	return response, nil
}

type quoteV1Fields QuoteResponse
type quoteV2Wire struct {
	quoteV1Fields
	Product *Product `json:"product"`
}

func DecodeQuoteV2Strict(raw []byte) (QuoteResponse, error) {
	var wire quoteV2Wire
	if err := unmarshalStrictRequired(raw, &wire, "request_id", "server_time", "schema_version", "status", "quote_id", "amount", "duration_code", "device_limit", "method", "expires_at", "product"); err != nil {
		return QuoteResponse{}, fmt.Errorf("quote v2 decode: %w", err)
	}
	response := QuoteResponse(wire.quoteV1Fields)
	response.Product, response.PaymentContract = wire.Product, 2
	if !validServerEnvelope(response.RequestID, response.ServerTime, response.SchemaVersion, response.Status) || !validUUID(response.QuoteID) ||
		response.Amount.AmountMinor <= 0 || response.Amount.Currency != "RUB" || response.DeviceLimit < 2 || !validDurationCodeV2(response.DurationCode) ||
		!validPaymentMethod(response.Method) || !validPaymentUTC(response.ExpiresAt) || (response.Product != nil && response.DeviceLimit != response.Product.DeviceLimit) {
		return QuoteResponse{}, fmt.Errorf("quote v2 invalid")
	}
	return response, nil
}

type paymentV1Fields PaymentResponse
type paymentV2Wire struct {
	paymentV1Fields
	Product            *Product         `json:"product"`
	CreditState        string           `json:"credit_state"`
	CreditReviewReason *string          `json:"credit_review_reason"`
	CreditedProduct    *CreditedProduct `json:"credited_product"`
}

func validCreditReviewReason(value string) bool {
	switch value {
	case "owner_unbound", "owner_changed", "target_unavailable", "target_expired", "target_period_changed", "extra_slot_unavailable":
		return true
	}
	return false
}

func DecodePaymentV2Strict(raw []byte) (PaymentResponse, error) {
	var wire paymentV2Wire
	if err := unmarshalStrictRequired(raw, &wire, "request_id", "server_time", "schema_version", "status", "payment_id", "payment_status", "checkout_reference", "credited_entitlement_revision", "access_application_state", "product", "credit_state", "credit_review_reason", "credited_product"); err != nil {
		return PaymentResponse{}, fmt.Errorf("payment v2 decode: %w", err)
	}
	response := PaymentResponse(wire.paymentV1Fields)
	response.Product, response.CreditState, response.CreditReviewReason, response.CreditedProduct, response.PaymentContract = wire.Product, wire.CreditState, wire.CreditReviewReason, wire.CreditedProduct, 2
	if !validServerEnvelope(response.RequestID, response.ServerTime, response.SchemaVersion, response.Status) || !validUUID(response.PaymentID) {
		return PaymentResponse{}, fmt.Errorf("payment v2 envelope invalid")
	}
	if !validPaymentStatus(response.PaymentStatus) {
		return PaymentResponse{}, fmt.Errorf("payment v2 status invalid")
	}
	if !validAccessApplicationState(response.AccessApplicationState) {
		return PaymentResponse{}, fmt.Errorf("payment v2 access state invalid")
	}
	switch response.CreditState {
	case "unapplied", "applied", "needs_review":
	default:
		return PaymentResponse{}, fmt.Errorf("payment v2 credit state invalid")
	}
	if (response.CreditReviewReason != nil && !validCreditReviewReason(*response.CreditReviewReason)) ||
		(response.CreditedEntitlementRevision != nil && !ValidRevision(*response.CreditedEntitlementRevision)) ||
		(response.CheckoutReference != nil && len(*response.CheckoutReference) > 256) {
		return PaymentResponse{}, fmt.Errorf("payment v2 receipt invalid")
	}
	return response, nil
}

func (c *Client) paymentQuery() string {
	if c.PaymentContract == 2 {
		return "payment_contract=2"
	}
	return ""
}

func (c *Client) decodePlans(raw []byte) (PlansResponse, error) {
	if c.PaymentContract == 2 {
		return DecodePlansV2Strict(raw)
	}
	return DecodePlansStrict(raw)
}

func (c *Client) decodeQuote(raw []byte) (QuoteResponse, error) {
	if c.PaymentContract == 2 {
		return DecodeQuoteV2Strict(raw)
	}
	return DecodeQuoteStrict(raw)
}

func (c *Client) decodePayment(raw []byte) (PaymentResponse, error) {
	if c.PaymentContract == 2 {
		return DecodePaymentV2Strict(raw)
	}
	return DecodePaymentStrict(raw)
}

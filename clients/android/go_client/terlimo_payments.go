package main

// S5 payment bridge vocabulary (FROZEN for the Kotlin workstream).
//
// Host -> native actions (the managed bridge parses the host JSON line and requires
// the existing v=1 / attempt_id envelope; every action is answered with exactly one
// `<action>_result` event that echoes attempt_id):
//
//	plans_list      no fields
//	quote_create    plan_id, duration_code, method, idempotency_key
//	payment_create  quote_id, idempotency_key
//	payment_get     payment_id
//
// Native -> host events, all shaped {"v":1,"attempt_id":...,"type":<event>,"state":
// "ok"|"error"} either with the contract payload or with a single bounded "code":
//
//	plans_list_result
//	  ok:    request_id, server_time, schema_version, plans_revision,
//	         plans:[{plan_id,title,duration_code,base_device_limit,
//	                 amount:{amount_minor,currency},methods:[...]}]
//	quote_create_result
//	  ok:    request_id, server_time, schema_version, quote_id,
//	         amount:{amount_minor,currency}, duration_code, device_limit, method,
//	         expires_at
//	payment_create_result / payment_get_result
//	  ok:    request_id, server_time, schema_version, payment_id, payment_status,
//	         checkout_reference (string|null), credited_entitlement_revision
//	         (string|null), access_application_state
//
// Bounded error codes: INVALID_REQUEST (host field violates the contract or the
// Idempotency-Key is absent), MOBILE_STATE_UNAVAILABLE, BUSY, TRANSPORT (any
// unclassifiable transport/service failure), PROVIDER_UNAVAILABLE (the distinct
// payment-provider outage state: 503/SERVICE_UNAVAILABLE on any payment-family
// action), plus the contract error codes from schemas/errors.json forwarded verbatim.
//
// The host owns Idempotency-Key values: native forwards them byte-for-byte on every
// attempt/retry and never generates, rotates, reuses for changed data or drops one.
// No handler here writes /me, the projection store, admission or entitlement state:
// payment status and credited_entitlement_revision are display data only, and any
// entitlement change is observed only through the next fresh /me read.

import (
	"context"
	"errors"
	"fmt"
	"os"

	"wg-turn-client/accountaccess"
	"wg-turn-client/servicechannel"
)

const (
	paymentActionPlansList     = "plans_list"
	paymentActionQuoteCreate   = "quote_create"
	paymentActionPaymentCreate = "payment_create"
	paymentActionPaymentGet    = "payment_get"

	paymentEventPlansList     = "plans_list_result"
	paymentEventQuoteCreate   = "quote_create_result"
	paymentEventPaymentCreate = "payment_create_result"
	paymentEventPaymentGet    = "payment_get_result"

	paymentCodeInvalidRequest      = "INVALID_REQUEST"
	paymentCodeMobileStateUnavail  = "MOBILE_STATE_UNAVAILABLE"
	paymentCodeTransport           = "TRANSPORT"
	paymentCodeProviderUnavailable = "PROVIDER_UNAVAILABLE"
)

// paymentResultCodeSet is the frozen schemas/errors.json ErrorCode enum. A server code
// outside this set is never forwarded; it collapses to TRANSPORT.
var paymentResultCodeSet = map[string]bool{
	"BAD_MESSAGE": true, "UNSUPPORTED_VERSION": true, "UNKNOWN_CRITICAL_FIELD": true,
	"PROOF_INVALID": true, "CHALLENGE_EXPIRED": true, "CHALLENGE_REUSED": true,
	"REPLAY_DETECTED": true, "WRONG_ENVIRONMENT": true, "WRONG_SCOPE": true,
	"SESSION_INVALID": true, "SESSION_EXPIRED": true, "TELEGRAM_REQUIRED": true,
	"TELEGRAM_LINK_PENDING": true, "TELEGRAM_LINK_REJECTED": true, "TRIAL_USED": true,
	"CHANNEL_CONFIRMATION_REQUIRED": true, "DEVICE_LIMIT_REACHED": true,
	"APPROVAL_REQUIRED": true, "DEVICE_REVOKED": true, "SUBSCRIPTION_EXPIRED": true,
	"SUBSCRIPTION_MISSING": true, "IDEMPOTENCY_CONFLICT": true, "REVISION_CONFLICT": true,
	"ACCESS_SYNC_PENDING": true, "OPERATION_PENDING": true, "OPERATION_FAILED": true,
	"OPERATION_UNKNOWN": true, "LEASE_CONFLICT": true, "GRANT_MISSING": true,
	"NODE_UNAVAILABLE": true, "READBACK_FAILED": true, "PAYMENT_NOT_FOUND": true,
	"PAYMENT_STATE_INVALID": true, "QUOTE_EXPIRED": true, "METHOD_UNAVAILABLE": true,
	"CHECKOUT_POLICY_DENIED": true, "RATE_LIMITED": true, "SERVICE_UNAVAILABLE": true,
	"NOT_FOUND": true, "ACCESS_DENIED": true, "INTERNAL": true,
}

// paymentEventForAction maps one frozen host action to its frozen result event.
func paymentEventForAction(action string) string {
	switch action {
	case paymentActionPlansList:
		return paymentEventPlansList
	case paymentActionQuoteCreate:
		return paymentEventQuoteCreate
	case paymentActionPaymentCreate:
		return paymentEventPaymentCreate
	case paymentActionPaymentGet:
		return paymentEventPaymentGet
	default:
		return ""
	}
}

// handlePaymentAction serves exactly the frozen S5 action set. It never touches the
// runner, the projection store or admission: payment reads are display-only.
func (m *managedMobile) handlePaymentAction(ctx context.Context, action bridgeMessage) {
	switch action.string("type") {
	case paymentActionPlansList:
		m.handlePlansList(ctx)
	case paymentActionQuoteCreate:
		m.handleQuoteCreate(ctx, action)
	case paymentActionPaymentCreate:
		m.handlePaymentCreate(ctx, action)
	case paymentActionPaymentGet:
		m.handlePaymentGet(ctx, action)
	}
}

func (m *managedMobile) handlePlansList(ctx context.Context) {
	fmt.Fprintln(os.Stderr, "plansdiag: BEGIN")
	event := paymentEventPlansList
	if m.client == nil {
		fmt.Fprintln(os.Stderr, "plansdiag: NO_CLIENT")
		m.sendPaymentError(event, paymentCodeMobileStateUnavail)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	plans, apiError, err := m.client.ListPlans(requestCtx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plansdiag: TRANSPORT")
		m.sendPaymentError(event, paymentTransportCode(err, false))
		return
	}
	if apiError != nil {
		fmt.Fprintln(os.Stderr, "plansdiag: API_ERROR")
		m.sendPaymentError(event, paymentAPIErrorCode(apiError, false))
		return
	}
	projected := make([]bridgeMessage, 0, len(plans.Plans))
	for _, plan := range plans.Plans {
		methods := make([]string, 0, len(plan.Methods))
		methods = append(methods, plan.Methods...)
		projected = append(projected, bridgeMessage{
			"plan_id":           plan.PlanID,
			"title":             plan.Title,
			"duration_code":     plan.DurationCode,
			"base_device_limit": plan.BaseDeviceLimit,
			"amount":            bridgeMessage{"amount_minor": plan.Amount.AmountMinor, "currency": plan.Amount.Currency},
			"methods":           methods,
		})
	}
	m.sendPaymentEvent(bridgeMessage{"type": event, "state": "ok",
		"request_id": plans.RequestID, "server_time": plans.ServerTime,
		"schema_version": plans.SchemaVersion, "plans_revision": plans.PlansRevision,
		"plans": projected})
	fmt.Fprintln(os.Stderr, "plansdiag: OK")
}

func (m *managedMobile) handleQuoteCreate(ctx context.Context, action bridgeMessage) {
	event := paymentEventQuoteCreate
	if m.client == nil {
		m.sendPaymentError(event, paymentCodeMobileStateUnavail)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	quote, apiError, err := m.client.CreateQuote(requestCtx,
		action.string("plan_id"), action.string("duration_code"), action.string("method"),
		action.string("idempotency_key"))
	if errors.Is(err, accountaccess.ErrInvalidRequest) {
		m.sendPaymentError(event, paymentCodeInvalidRequest)
		return
	}
	if err != nil {
		m.sendPaymentError(event, paymentTransportCode(err, true))
		return
	}
	if apiError != nil {
		m.sendPaymentError(event, paymentAPIErrorCode(apiError, true))
		return
	}
	m.sendPaymentEvent(bridgeMessage{"type": event, "state": "ok",
		"request_id": quote.RequestID, "server_time": quote.ServerTime,
		"schema_version": quote.SchemaVersion, "quote_id": quote.QuoteID,
		"amount":        bridgeMessage{"amount_minor": quote.Amount.AmountMinor, "currency": quote.Amount.Currency},
		"duration_code": quote.DurationCode, "device_limit": quote.DeviceLimit,
		"method": quote.Method, "expires_at": quote.ExpiresAt})
}

func (m *managedMobile) handlePaymentCreate(ctx context.Context, action bridgeMessage) {
	event := paymentEventPaymentCreate
	if m.client == nil {
		m.sendPaymentError(event, paymentCodeMobileStateUnavail)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	payment, apiError, err := m.client.CreatePayment(requestCtx,
		action.string("quote_id"), action.string("idempotency_key"))
	if errors.Is(err, accountaccess.ErrInvalidRequest) {
		m.sendPaymentError(event, paymentCodeInvalidRequest)
		return
	}
	if err != nil {
		m.sendPaymentError(event, paymentTransportCode(err, true))
		return
	}
	if apiError != nil {
		m.sendPaymentError(event, paymentAPIErrorCode(apiError, true))
		return
	}
	m.sendPaymentEvent(paymentResultMessage(event, payment))
}

func (m *managedMobile) handlePaymentGet(ctx context.Context, action bridgeMessage) {
	event := paymentEventPaymentGet
	if m.client == nil {
		m.sendPaymentError(event, paymentCodeMobileStateUnavail)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	payment, apiError, err := m.client.GetPayment(requestCtx, action.string("payment_id"))
	if errors.Is(err, accountaccess.ErrInvalidRequest) {
		m.sendPaymentError(event, paymentCodeInvalidRequest)
		return
	}
	if err != nil {
		m.sendPaymentError(event, paymentTransportCode(err, true))
		return
	}
	if apiError != nil {
		m.sendPaymentError(event, paymentAPIErrorCode(apiError, true))
		return
	}
	m.sendPaymentEvent(paymentResultMessage(event, payment))
}

// paymentResultMessage projects one payment response verbatim into the frozen event
// fields. Nullable fields stay null when absent; nothing is fabricated or completed.
func paymentResultMessage(event string, payment accountaccess.PaymentResponse) bridgeMessage {
	return bridgeMessage{"type": event, "state": "ok",
		"request_id": payment.RequestID, "server_time": payment.ServerTime,
		"schema_version":                payment.SchemaVersion,
		"payment_id":                    payment.PaymentID,
		"payment_status":                payment.PaymentStatus,
		"checkout_reference":            payment.CheckoutReference,
		"credited_entitlement_revision": payment.CreditedEntitlementRevision,
		"access_application_state":      payment.AccessApplicationState}
}

// paymentAPIErrorCode maps one bounded server error envelope to the host vocabulary.
// SERVICE_UNAVAILABLE on a payment-family action becomes the distinct
// PROVIDER_UNAVAILABLE state (for example PLATEGA_ENABLED=false); every other contract
// error code is forwarded verbatim, and anything outside the frozen enum is TRANSPORT.
func paymentAPIErrorCode(apiError *accountaccess.ErrorResponse, provider bool) string {
	if apiError == nil {
		return paymentCodeTransport
	}
	if provider && apiError.Code == "SERVICE_UNAVAILABLE" {
		return paymentCodeProviderUnavailable
	}
	if paymentResultCodeSet[apiError.Code] {
		return apiError.Code
	}
	return paymentCodeTransport
}

// paymentTransportCode classifies a transport/service failure. A bounded service-peer
// SERVICE_UNAVAILABLE frame on a payment-family action is the provider-unavailable
// state as well; every other transport failure is TRANSPORT. Raw transport text never
// reaches the host.
func paymentTransportCode(err error, provider bool) string {
	var serviceErr *servicechannel.ServiceError
	if errors.As(err, &serviceErr) && provider && serviceErr.Code == "SERVICE_UNAVAILABLE" {
		return paymentCodeProviderUnavailable
	}
	return paymentCodeTransport
}

func (m *managedMobile) sendPaymentEvent(message bridgeMessage) {
	if m.bridge != nil {
		_ = m.bridge.send(message)
	}
}

func (m *managedMobile) sendPaymentError(event, code string) {
	m.sendPaymentEvent(bridgeMessage{"type": event, "state": "error", "code": code})
}

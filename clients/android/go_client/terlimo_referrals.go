package main

// Referral actions use the existing managed mobile dispatcher. The host persists
// mutation intent before sending and owns replay with the original key. These
// frames never authorize a trial, purchase, login or data-plane connection.
//
// Host commands:
//   referral_info: client_request_id, account_ref
//   referral_candidate_set: client_request_id, idempotency_key, code
//   referral_candidate_clear: client_request_id, idempotency_key
//   request_telegram_registration (opt-in only): client_request_id,
//     idempotency_key, referral_candidate_id
//
// Results echo the host correlation, mutation key and optional candidate id.
// Candidate/info results carry the strictly decoded accountaccess server
// envelope as payload. Keyed registration also returns its exact validated
// envelope; a registered receipt is accompanied by a fresh authenticated /me.

import (
	"context"
	"net/http"

	"wg-turn-client/accountaccess"
)

const (
	referralActionInfo           = "referral_info"
	referralActionCandidateSet   = "referral_candidate_set"
	referralActionCandidateClear = "referral_candidate_clear"
	referralActionRegistration   = "request_telegram_registration"
	referralEventInfo            = "referral_info_result"
	referralEventCandidate       = "referral_candidate_result"
)

type managedReferralRequest struct {
	Action         string
	RequestID      string
	IdempotencyKey string
	Code           string
	CandidateID    string
	AccountRef     string
}

func validReferralCode(code string) bool {
	if len(code) < 1 || len(code) > 32 {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// Opaque identities are bounded printable ASCII. They are never interpreted as
// credentials or endpoint paths by this bridge.
func validReferralIdentity(value string, min int) bool {
	if len(value) < min || len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 33 || value[i] > 126 {
			return false
		}
	}
	return true
}

func validReferralServerRequestID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func hasReferralRegistrationFields(message bridgeMessage) bool {
	for _, key := range []string{"referral_candidate_id", "idempotency_key", "client_request_id"} {
		if _, exists := message[key]; exists {
			return true
		}
	}
	return false
}

func parseReferralRequest(message bridgeMessage) (managedReferralRequest, bool) {
	request := managedReferralRequest{
		Action: message.string("type"), RequestID: message.string("client_request_id"),
		IdempotencyKey: message.string("idempotency_key"), Code: message.string("code"),
		CandidateID: message.string("referral_candidate_id"), AccountRef: message.string("account_ref"),
	}
	if !validReferralIdentity(request.RequestID, 1) {
		return request, false
	}
	switch request.Action {
	case referralActionInfo:
		return request, validReferralIdentity(request.AccountRef, 1)
	case referralActionCandidateSet:
		return request, validReferralIdentity(request.IdempotencyKey, 16) && validReferralCode(request.Code)
	case referralActionCandidateClear:
		return request, validReferralIdentity(request.IdempotencyKey, 16)
	case referralActionRegistration:
		return request, validReferralIdentity(request.IdempotencyKey, 16) && validReferralIdentity(request.CandidateID, 1)
	default:
		return request, false
	}
}

func (request managedReferralRequest) resultFrame() bridgeMessage {
	event := referralEventCandidate
	operation := "set"
	switch request.Action {
	case referralActionInfo:
		event, operation = referralEventInfo, "get"
	case referralActionCandidateClear:
		operation = "clear"
	case referralActionRegistration:
		event, operation = "telegram_registration", "registration"
	}
	message := bridgeMessage{"type": event, "operation": operation}
	// Even a malformed inbound frame can only echo bounded correlation fields.
	if validReferralIdentity(request.RequestID, 1) {
		message["client_request_id"] = request.RequestID
	}
	if validReferralIdentity(request.IdempotencyKey, 16) {
		message["idempotency_key"] = request.IdempotencyKey
	}
	if validReferralIdentity(request.CandidateID, 1) {
		message["referral_candidate_id"] = request.CandidateID
	}
	if validReferralIdentity(request.AccountRef, 1) {
		message["account_ref"] = request.AccountRef
	}
	return message
}

func (b *managedBridge) sendReferralError(request managedReferralRequest, code string, retryable bool) {
	message := request.resultFrame()
	message["state"], message["code"], message["retryable"] = "error", code, retryable
	_ = b.send(message)
}

func (b *managedBridge) queueReferral(message bridgeMessage) {
	request, valid := parseReferralRequest(message)
	if !valid {
		b.sendReferralError(request, "INVALID_REQUEST", false)
		return
	}
	select {
	case b.referrals <- request:
	default:
		// Mutation retries retain the original key; queue pressure cannot silently
		// drop the intent or imply that it was accepted by the server.
		b.sendReferralError(request, "BUSY", true)
	}
}

func (m *managedMobile) handleReferralAction(ctx context.Context, request managedReferralRequest) {
	if ctx.Err() != nil || m.bridge == nil {
		return
	}
	if request.Action == referralActionRegistration {
		m.handleReferralRegistration(ctx, request)
		return
	}
	if m.client == nil || m.session == nil {
		m.bridge.sendReferralError(request, "MOBILE_STATE_UNAVAILABLE", true)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	if err := m.session.Ensure(requestCtx); err != nil {
		m.bridge.sendReferralError(request, "TRANSPORT", true)
		return
	}
	// Reuse the authenticated client and freeze the bearer for this foreground
	// request. A runner refresh/account switch must not relabel an old response.
	bearer, subject, generation, current := m.session.CachedBearer()
	if !current {
		m.bridge.sendReferralError(request, "SESSION_CHANGED", true)
		return
	}
	requestCtx = accountaccess.WithExistingBearer(requestCtx, bearer)
	// The existing observer exposes status without raw body or a second HTTP
	// implementation. Keep capture scoped to this request rather than mutating
	// the shared runner client or observing an unrelated concurrent response.
	client := *m.client
	responseStatus := 0
	previousObserver := client.OnResponse
	client.OnResponse = func(path string, status int) {
		if path == "/referral/candidate" {
			responseStatus = status
		}
		if previousObserver != nil {
			previousObserver(path, status)
		}
	}
	stillCurrent := func() bool {
		tokenAfter, subjectAfter, generationAfter, current := m.session.CachedBearer()
		return current && tokenAfter == bearer && subjectAfter == subject && generationAfter == generation
	}
	var payload any
	var apiError *accountaccess.ErrorResponse
	var err error
	switch request.Action {
	case referralActionInfo:
		if subject.AccountRef == "" || subject.AccountRef != request.AccountRef {
			m.bridge.sendReferralError(request, "REFERRAL_ACCOUNT_MISMATCH", false)
			return
		}
		// Fresh authenticated /me is sufficient. No paid/trial, gateway, admission
		// or data grant condition is introduced for own-code reads.
		me, failure, readErr := client.GetMe(requestCtx)
		if !stillCurrent() {
			m.bridge.sendReferralError(request, "SESSION_CHANGED", true)
			return
		}
		if readErr != nil || failure != nil {
			m.sendReferralFailure(request, failure, readErr, 0)
			return
		}
		if !me.TelegramLinked || me.AccountRef == nil || *me.AccountRef != request.AccountRef {
			m.bridge.sendReferralError(request, "REFERRAL_ACCOUNT_MISMATCH", false)
			return
		}
		payload, apiError, err = client.GetReferralInfo(requestCtx, request.AccountRef)
	case referralActionCandidateSet:
		payload, apiError, err = client.SetReferralCandidate(requestCtx, request.Code, request.IdempotencyKey)
	case referralActionCandidateClear:
		payload, apiError, err = client.ClearReferralCandidate(requestCtx, request.IdempotencyKey)
	default:
		m.bridge.sendReferralError(request, "INVALID_REQUEST", false)
		return
	}
	if ctx.Err() != nil {
		return
	}
	if !stillCurrent() {
		m.bridge.sendReferralError(request, "SESSION_CHANGED", true)
		return
	}
	if err != nil || apiError != nil {
		m.sendReferralFailure(request, apiError, err, responseStatus)
		return
	}
	message := request.resultFrame()
	message["state"], message["payload"] = "ok", payload
	_ = m.bridge.send(message)
}

func (m *managedMobile) sendReferralFailure(request managedReferralRequest, apiError *accountaccess.ErrorResponse, err error, responseStatus int) {
	if err != nil {
		// Transport, malformed success and foreign-account replies all retain the
		// host intent. Raw error strings/bodies never enter the bridge or logs.
		m.bridge.sendReferralError(request, "TRANSPORT", true)
		return
	}
	code := "REFERRAL_FAILED"
	if apiError != nil {
		switch apiError.Code {
		case "REFERRAL_CODE_INVALID", "BAD_MESSAGE", "REFERRAL_CANDIDATE_LOCKED",
			"IDEMPOTENCY_CONFLICT", "REFERRAL_HISTORY_PENDING", "ACCESS_DENIED",
			"REGISTRATION_DISABLED", "REGISTRATION_EXPIRED",
			"SERVICE_UNAVAILABLE", "SESSION_EXPIRED", "SESSION_INVALID", "NOT_FOUND", "INTERNAL":
			code = apiError.Code
		}
		message := request.resultFrame()
		message["state"], message["code"] = "error", code
		message["retryable"] = apiError.Retryable
		if validReferralServerRequestID(apiError.RequestID) {
			message["request_id"] = apiError.RequestID
		}
		if responseStatus > 0 {
			message["http_status"] = responseStatus
		}
		// Only the contract's exact decoded no-mutation rejection permits a typo
		// replacement. A status alone, locked/conflict or gateway error cannot
		// close an unknown candidate mutation.
		if request.Action == referralActionCandidateSet && !apiError.Retryable && validReferralServerRequestID(apiError.RequestID) &&
			((responseStatus == http.StatusNotFound && apiError.Code == "REFERRAL_CODE_INVALID") ||
				(responseStatus == http.StatusBadRequest && apiError.Code == "BAD_MESSAGE")) {
			message["definitive_rejection"] = true
		}
		if apiError.RetryAfterMS != nil {
			message["retry_after_ms"] = *apiError.RetryAfterMS
		}
		_ = m.bridge.send(message)
		return
	}
	m.bridge.sendReferralError(request, code, true)
}

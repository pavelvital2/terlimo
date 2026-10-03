package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"wg-turn-client/accountaccess"
)

// Capture only the existing authenticated client's registration response. The
// accountaccess keyed API remains the request builder and sole response validator;
// its DTO omits the outer success envelope, which the host also needs to persist.
type referralRegistrationCapture struct {
	inner   accountaccess.Doer
	payload json.RawMessage
	status  int
}

func (capture *referralRegistrationCapture) Do(request *http.Request) (*http.Response, error) {
	response, err := capture.inner.Do(request)
	if err != nil {
		return response, err
	}
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("registration response unavailable")
	}
	if request.URL.Path != "/api/mobile/v1/registration/telegram/link" {
		return response, nil
	}
	// No raw response/token is logged, persisted here or exposed before the keyed
	// API validates it. Bound capture below the managed bridge frame budget.
	const limit = managedBridgeLimit / 2
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	_ = response.Body.Close()
	if readErr != nil || len(raw) > limit {
		return nil, fmt.Errorf("registration response unreadable")
	}
	capture.payload, capture.status = raw, response.StatusCode
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

func (m *managedMobile) handleReferralRegistration(ctx context.Context, request managedReferralRequest) {
	if ctx.Err() != nil || m.bridge == nil {
		return
	}
	if m.client == nil || m.client.HTTP == nil || m.session == nil {
		m.bridge.sendReferralError(request, "MOBILE_STATE_UNAVAILABLE", true)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, mobileHTTPTimeout)
	defer cancel()
	if err := m.session.Ensure(requestCtx); err != nil {
		m.bridge.sendReferralError(request, "TRANSPORT", true)
		return
	}
	bearer, subject, generation, current := m.session.CachedBearer()
	if !current {
		m.bridge.sendReferralError(request, "SESSION_CHANGED", true)
		return
	}
	if request.AccountRef != "" && request.AccountRef != subject.AccountRef {
		m.bridge.sendReferralError(request, "REFERRAL_ACCOUNT_MISMATCH", false)
		return
	}
	pinned := accountaccess.WithExistingBearer(requestCtx, bearer)
	client := *m.client
	capture := &referralRegistrationCapture{inner: client.HTTP}
	client.HTTP = capture
	link, apiError, err := client.RequestRegistrationLinkWithCandidateKey(pinned,
		accountaccess.RegistrationReferralCandidate{CandidateID: request.CandidateID}, request.IdempotencyKey)
	if ctx.Err() != nil {
		return
	}
	stillCurrent := func(token string, owner accountaccess.Subject, sessionGeneration string) bool {
		afterToken, afterOwner, afterGeneration, ok := m.session.CachedBearer()
		return ok && afterToken == token && afterOwner == owner && afterGeneration == sessionGeneration
	}
	if !stillCurrent(bearer, subject, generation) {
		m.bridge.sendReferralError(request, "SESSION_CHANGED", true)
		return
	}
	if err != nil {
		m.sendReferralFailure(request, apiError, err, capture.status)
		return
	}
	if apiError != nil {
		if proof, terminal := apiError.ReferralRegistrationExpiry(); terminal &&
			validReferralServerRequestID(apiError.RequestID) && proof.CandidateID == request.CandidateID && proof.IdempotencyKey == request.IdempotencyKey {
			message := request.resultFrame()
			message["state"], message["code"], message["retryable"] = "error", "REGISTRATION_EXPIRED", false
			message["http_status"], message["request_id"] = http.StatusGone, apiError.RequestID
			message["referral_registration_expired"], message["payload"] = true, capture.payload
			_ = m.bridge.send(message)
			return
		}
		m.sendReferralFailure(request, apiError, nil, capture.status)
		return
	}
	message := request.resultFrame()
	message["payload"] = capture.payload
	switch link.State {
	case "pending":
		message["state"], message["deep_link"] = "pending", link.DeepLink
		if link.ExpiresAt != nil {
			message["expires_at"] = *link.ExpiresAt
		}
	case "registered":
		// A receipt's account is not a login credential. Reauthenticate through
		// the existing session owner, then read strict /me before exposing it as
		// terminal attribution. No admission/catalog/access mutation happens here.
		m.session.Refresh()
		if ensureErr := m.session.Ensure(requestCtx); ensureErr != nil {
			m.bridge.sendReferralError(request, "TRANSPORT", true)
			return
		}
		freshBearer, freshSubject, freshGeneration, ok := m.session.CachedBearer()
		receipt := link.ReferralAttribution
		if !ok || receipt == nil || freshSubject.AccountRef == "" ||
			freshSubject.AccountRef != receipt.AccountRef ||
			(request.AccountRef != "" && request.AccountRef != freshSubject.AccountRef) {
			m.bridge.sendReferralError(request, "REFERRAL_ACCOUNT_MISMATCH", false)
			return
		}
		freshCtx := accountaccess.WithExistingBearer(requestCtx, freshBearer)
		me, meError, readErr := m.client.GetMe(freshCtx)
		if ctx.Err() != nil {
			return
		}
		if !stillCurrent(freshBearer, freshSubject, freshGeneration) {
			m.bridge.sendReferralError(request, "SESSION_CHANGED", true)
			return
		}
		if readErr != nil || meError != nil {
			m.sendReferralFailure(request, meError, readErr, 0)
			return
		}
		verifiedAccount := false
		switch me.AccountState {
		case "VERIFIED_NO_ENTITLEMENT", "VERIFIED_NO_SLOT", "ACTIVE_TRIAL", "ACTIVE_PAID", "EXPIRED":
			verifiedAccount = true
		}
		if !verifiedAccount || !me.TelegramLinked || me.AccountRef == nil || *me.AccountRef != receipt.AccountRef {
			m.bridge.sendReferralError(request, "REFERRAL_ACCOUNT_MISMATCH", false)
			return
		}
		message["state"], message["account_ref"], message["fresh_me"] = "registered", freshSubject.AccountRef, me
		if request.AccountRef != "" {
			message["captured_account_ref"] = request.AccountRef
		}
	default:
		m.bridge.sendReferralError(request, "TRANSPORT", true)
		return
	}
	if ctx.Err() == nil {
		_ = m.bridge.send(message)
	}
}

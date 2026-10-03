package accountaccess

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

const registrationKey = "registration-key-0001"
const registrationProof = `{"candidate_id":"12345678-1234-1234-1234-123456789abc","registration_id":"aaaaaaaa-1234-1234-1234-123456789abc","idempotency_key":"registration-key-0001"}`
const attributionProof = `{"receipt_id":"bbbbbbbb-1234-1234-1234-123456789abc","account_ref":"cccccccc-1234-1234-1234-123456789abc","candidate_id":"12345678-1234-1234-1234-123456789abc","registration_id":"aaaaaaaa-1234-1234-1234-123456789abc","idempotency_key":"registration-key-0001","state":"attached","reason":null}`
const registrationPendingBase = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"ok","registration":{"state":"pending","token":"tok123","bot_username":"terlimo_bot","deep_link":"https://t.me/terlimo_bot?start=tok123","expires_at":"2026-10-03T00:10:00Z","expires_in":600}}`
const registrationExpiry = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"error","code":"REGISTRATION_EXPIRED","retryable":false,"details":{"state":"expired","referral_registration":` + registrationProof + `}}`

func withRegistrationProof(base, field, proof string) string {
	return strings.TrimSuffix(base, "}}") + `,"` + field + `":` + proof + `}}`
}
func TestReferralRegistrationProofStrict(t *testing.T) {
	pending := withRegistrationProof(registrationPendingBase, "referral_registration", registrationProof)
	registered := withRegistrationProof(referralRegistered, "referral_attribution", attributionProof)
	for _, raw := range []string{pending, registered, strings.Replace(registered, `"state":"attached","reason":null`, `"state":"rejected","reason":"self"`, 1)} {
		if _, err := DecodeRegistrationLinkStrict([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	bad := []string{
		withRegistrationProof(registrationPendingBase, "referral_attribution", attributionProof),
		withRegistrationProof(referralRegistered, "referral_registration", registrationProof),
		strings.Replace(pending, `"candidate_id":"`+referralTestID+`"`, `"candidate_id":null`, 1),
		strings.Replace(pending, `"candidate_id":"`+referralTestID+`",`, ``, 1),
		strings.Replace(pending, registrationKey, "", 1),
		strings.Replace(pending, `"idempotency_key"`, `"unknown"`, 1),
		strings.Replace(pending, `"idempotency_key":"`+registrationKey+`"`, `"idempotency_key":"`+registrationKey+`","idempotency_key":"`+registrationKey+`"`, 1),
		strings.Replace(registered, `"account_ref":"cccccccc-1234-1234-1234-123456789abc"`, `"account_ref":"bad"`, 1),
		strings.Replace(registered, `"reason":null`, `"reason":"self"`, 1),
		strings.Replace(registered, `"state":"attached"`, `"state":"rejected"`, 1),
		withRegistrationProof(registered, "referral_attribution_receipt_id", `"aaaaaaaa-1234-1234-1234-123456789abc"`),
	}
	for i, raw := range bad {
		if _, err := DecodeRegistrationLinkStrict([]byte(raw)); err == nil {
			t.Fatalf("accepted bad proof %d", i)
		}
	}
	matching := withRegistrationProof(registered, "referral_attribution_receipt_id", `"bbbbbbbb-1234-1234-1234-123456789abc"`)
	if _, err := DecodeRegistrationLinkStrict([]byte(matching)); err != nil {
		t.Fatal(err)
	}
	var r ReferralAttributionReceipt
	if err := json.Unmarshal([]byte(attributionProof), &r); err != nil {
		t.Fatal(err)
	}
	if !r.Matches(r.AccountRef, r.CandidateID, r.RegistrationID, r.IdempotencyKey) || r.Matches(referralTestID, r.CandidateID, r.RegistrationID, r.IdempotencyKey) || r.Matches(r.AccountRef, r.CandidateID, referralTestID, r.IdempotencyKey) {
		t.Fatal("receipt correlation")
	}
}
func TestReferralRegistrationKeyedReplay(t *testing.T) {
	ctx := context.Background()
	pending := withRegistrationProof(registrationPendingBase, "referral_registration", registrationProof)
	candidate := RegistrationReferralCandidate{referralTestID}
	c := Client{BaseURL: "https://service.invalid/api/mobile/v1"}
	c.HTTP = referralDoer(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.Path != "/api/mobile/v1/registration/telegram/link" || r.Header.Get("Idempotency-Key") != registrationKey || string(body) != `{"referral_candidate_id":"`+referralTestID+`"}` {
			t.Fatal("wrong keyed request")
		}
		return referralReply(pending, 200), nil
	})
	for i := 0; i < 2; i++ {
		v, e, err := c.RequestRegistrationLinkWithCandidateKey(ctx, candidate, registrationKey)
		if err != nil || e != nil || v.ReferralRegistration == nil {
			t.Fatal(err, e)
		}
	}
	for _, raw := range []string{registrationPendingBase, referralRegistered, strings.Replace(pending, registrationKey, "registration-key-0002", 1), strings.Replace(pending, referralTestID, "dddddddd-1234-1234-1234-123456789abc", 1), strings.Replace(withRegistrationProof(referralRegistered, "referral_attribution", attributionProof), registrationKey, "registration-key-0002", 1)} {
		c.HTTP = referralDoer(func(r *http.Request) (*http.Response, error) { return referralReply(raw, 200), nil })
		if v, _, err := c.RequestRegistrationLinkWithCandidateKey(ctx, candidate, registrationKey); err == nil || v.State != "" {
			t.Fatal("uncorrelated success")
		}
	}
	c.HTTP = referralDoer(func(r *http.Request) (*http.Response, error) { t.Fatal("invalid key sent"); return nil, nil })
	if _, _, err := c.RequestRegistrationLinkWithCandidateKey(ctx, candidate, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}
	c.HTTP = referralDoer(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := c.RequestRegistrationLinkWithCandidateKey(canceled, candidate, registrationKey); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Source-compatible candidate-only call is deliberately still unkeyed.
	c.HTTP = referralDoer(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Idempotency-Key") != "" {
			t.Fatal("legacy key invented")
		}
		return referralReply(registrationPendingBase, 200), nil
	})
	if _, _, err := c.RequestRegistrationLinkWithCandidate(ctx, candidate); err != nil {
		t.Fatal(err)
	}
}
func TestReferralRegistrationExpiryCorrelation(t *testing.T) {
	c := Client{BaseURL: "https://service.invalid/api/mobile/v1"}
	ctx := context.Background()
	candidate := RegistrationReferralCandidate{referralTestID}
	c.HTTP = referralDoer(func(r *http.Request) (*http.Response, error) { return referralReply(registrationExpiry, 410), nil })
	_, e, err := c.RequestRegistrationLinkWithCandidateKey(ctx, candidate, registrationKey)
	if err != nil {
		t.Fatal(err)
	}
	proof, ok := e.ReferralRegistrationExpiry()
	if !ok || !proof.Matches(referralTestID, "aaaaaaaa-1234-1234-1234-123456789abc", registrationKey) {
		t.Fatal("missing expiry proof")
	}
	for _, raw := range []string{strings.Replace(registrationExpiry, `"code":"REGISTRATION_EXPIRED"`, `"code":"REGISTRATION_EXPIRED","code":"REGISTRATION_EXPIRED"`, 1), strings.Replace(registrationExpiry, registrationKey, "registration-key-0002", 1), strings.Replace(registrationExpiry, `"retryable":false`, `"retryable":true`, 1), strings.Replace(registrationExpiry, `"details":{"state":"expired","referral_registration":`+registrationProof+`}`, `"details":{}`, 1)} {
		c.HTTP = referralDoer(func(r *http.Request) (*http.Response, error) { return referralReply(raw, 410), nil })
		_, e, err := c.RequestRegistrationLinkWithCandidateKey(ctx, candidate, registrationKey)
		_, ok := e.ReferralRegistrationExpiry()
		if err == nil || ok {
			t.Fatal("uncorrelated expiry terminal")
		}
	}
}

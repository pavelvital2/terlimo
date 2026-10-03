package accountaccess

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

const referralTestID = "12345678-1234-1234-1234-123456789abc"
const referralPending = `{"request_id":"0123456789abcdef0123456789abcdef","candidate":{"id":"12345678-1234-1234-1234-123456789abc","state":"pending","code":"Ab12"}}`
const referralCleared = `{"request_id":"0123456789abcdef0123456789abcdef","candidate":{"state":"cleared"}}`
const referralInfoFixture = `{"request_id":"0123456789abcdef0123456789abcdef","referral":{"account_ref":"12345678-1234-1234-1234-123456789abc","code":"Ab12","links":{"telegram":"https://t.me/terlimo_vpn_wdtt_bot?start=ref_uAb12","web":"https://terlimo.xyz/?ref=uAb12"},"attribution":{"state":"none","receipt_id":null,"reason":null},"benefits":{"trial_bonus_days":3,"discount":{"currency":"RUB","amount_minor":10000,"state":"eligible"}},"rewards":{"waiting_days":0,"applied_days":0},"terms_version":"referral-20261003-v1"}}`
const referralRegistered = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"ok","registration":{"state":"registered"}}`

type referralDoer func(*http.Request) (*http.Response, error)

func (f referralDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }
func referralReply(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestReferralStrictWire(t *testing.T) {
	for _, raw := range []string{referralPending, referralCleared} {
		if _, err := DecodeReferralCandidateStrict([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := DecodeReferralInfoStrict([]byte(referralInfoFixture)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"fraction":               strings.Replace(referralInfoFixture, `"amount_minor":10000`, `"amount_minor":1.5`, 1),
		"coercion":               strings.Replace(referralInfoFixture, `"waiting_days":0`, `"waiting_days":"0"`, 1),
		"nullzero":               strings.Replace(referralInfoFixture, `"waiting_days":0`, `"waiting_days":null`, 1),
		"negative":               strings.Replace(referralInfoFixture, `"waiting_days":0`, `"waiting_days":-1`, 1),
		"missingzero":            strings.Replace(referralInfoFixture, `"waiting_days":0,`, ``, 1),
		"duplicate":              strings.Replace(referralInfoFixture, `"waiting_days":0`, `"waiting_days":0,"waiting_days":7`, 1),
		"unknown":                strings.Replace(referralInfoFixture, `"waiting_days":0`, `"waiting_days":0,"other":1`, 1),
		"wrongcase":              strings.Replace(referralInfoFixture, `"waiting_days"`, `"Waiting_days"`, 1),
		"foreignorigin":          strings.Replace(referralInfoFixture, "https://terlimo.xyz/", "https://evil.test/", 1),
		"foreigncode":            strings.Replace(referralInfoFixture, "?ref=uAb12", "?ref=uOther", 1),
		"badaccount":             strings.Replace(referralInfoFixture, referralTestID, "not-uuid", 1),
		"badcode":                strings.ReplaceAll(referralInfoFixture, "Ab12", "Код"),
		"overflow":               strings.Replace(referralInfoFixture, `"waiting_days":0`, `"waiting_days":9223372036854775808`, 1),
		"currency":               strings.Replace(referralInfoFixture, `"RUB"`, `"USD"`, 1),
		"terms":                  strings.Replace(referralInfoFixture, ReferralTermsVersion, "future", 1),
		"nonewithreceipt":        strings.Replace(referralInfoFixture, `"receipt_id":null`, `"receipt_id":"`+referralTestID+`"`, 1),
		"attachedwithoutreceipt": strings.Replace(referralInfoFixture, `"state":"none"`, `"state":"attached"`, 1),
		"rejectwithoutreason":    strings.Replace(referralInfoFixture, `"state":"none"`, `"state":"rejected"`, 1),
		"unknownstate":           strings.Replace(referralInfoFixture, `"state":"eligible"`, `"state":"yes"`, 1),
		"trailing":               referralInfoFixture + `garbage`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeReferralInfoStrict([]byte(raw)); err == nil {
				t.Fatal("accepted invalid wire")
			}
		})
	}
	for _, raw := range []string{
		strings.Replace(referralPending, referralTestID, "bad", 1),
		strings.Replace(referralPending, `"code":"Ab12"`, `"code":"https://terlimo.xyz"`, 1),
		strings.Replace(referralPending, `"pending"`, `"cleared"`, 1),
		strings.Replace(referralCleared, `"cleared"`, `"pending"`, 1),
		strings.Replace(referralPending, `"code":"Ab12"`, `"code":"`+strings.Repeat("a", 33)+`"`, 1),
	} {
		if _, err := DecodeReferralCandidateStrict([]byte(raw)); err == nil {
			t.Fatal("accepted malformed candidate")
		}
	}
	attached := strings.Replace(referralInfoFixture, `"state":"none","receipt_id":null,"reason":null`, `"state":"attached","receipt_id":"`+referralTestID+`","reason":null`, 1)
	if _, err := DecodeReferralInfoStrict([]byte(attached)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		strings.Replace(attached, `"reason":null`, `"reason":"self"`, 1),
		strings.Replace(referralInfoFixture, `"trial_bonus_days":3`, `"trial_bonus_days":3e0`, 1),
		strings.Replace(referralInfoFixture, `"state":"none","receipt_id":null,"reason":null`, `"state":"rejected","receipt_id":"`+referralTestID+`","reason":"unknown"`, 1),
	} {
		if _, err := DecodeReferralInfoStrict([]byte(raw)); err == nil {
			t.Fatal("invalid state/integer accepted")
		}
	}
	for _, state := range []string{"eligible", "reserved", "consumed", "ineligible", "history_pending"} {
		if _, err := DecodeReferralInfoStrict([]byte(strings.Replace(referralInfoFixture, `"eligible"`, `"`+state+`"`, 1))); err != nil {
			t.Fatal(err)
		}
	}
	for _, reason := range []string{"self", "already_attributed", "ineligible", "invalid"} {
		raw := strings.Replace(referralInfoFixture, `"state":"none","receipt_id":null,"reason":null`, `"state":"rejected","receipt_id":"`+referralTestID+`","reason":"`+reason+`"`, 1)
		if _, err := DecodeReferralInfoStrict([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
}
func TestReferralRegistrationCompatibility(t *testing.T) {
	if v, err := DecodeRegistrationLinkStrict([]byte(referralRegistered)); err != nil || v.ReferralAttributionReceiptID != nil {
		t.Fatal(err)
	}
	with := strings.Replace(referralRegistered, `"state":"registered"`, `"state":"registered","referral_attribution_receipt_id":"`+referralTestID+`"`, 1)
	if v, err := DecodeRegistrationLinkStrict([]byte(with)); err != nil || v.ReferralAttributionReceiptID == nil {
		t.Fatal(err)
	}
	for _, raw := range []string{strings.Replace(with, referralTestID, "bad", 1), strings.Replace(with, `"registered"`, `"pending"`, 1), strings.Replace(with, `"state":"registered"`, `"state":"registered","state":"registered"`, 1)} {
		if _, err := DecodeRegistrationLinkStrict([]byte(raw)); err == nil {
			t.Fatal("bad registration receipt accepted")
		}
	}
}
func TestReferralClientRequests(t *testing.T) {
	key := "referral-key-0001"
	steps := []struct{ method, path, body, key, reply string }{
		{"POST", "/api/mobile/v1/referral/candidate", `{"code":"Ab12"}`, key, referralPending},
		{"DELETE", "/api/mobile/v1/referral/candidate", "", key, referralCleared},
		{"GET", "/api/mobile/v1/referral", "", "", referralInfoFixture},
		{"POST", "/api/mobile/v1/registration/telegram/link", `{}`, "", referralRegistered},
		{"POST", "/api/mobile/v1/registration/telegram/link", `{"referral_candidate_id":"` + referralTestID + `"}`, "", referralRegistered},
	}
	n := 0
	c := Client{BaseURL: "https://service.invalid/api/mobile/v1", Tokens: &plansTokenSource{value: "test-bearer"}}
	c.HTTP = referralDoer(func(r *http.Request) (*http.Response, error) {
		s := steps[n]
		n++
		body := []byte{}
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		if r.Method != s.method || r.URL.Path != s.path || string(body) != s.body || r.Header.Get("Idempotency-Key") != s.key || r.Header.Get("Authorization") != "Bearer test-bearer" {
			t.Fatalf("request mismatch step %d", n)
		}
		return referralReply(s.reply, 200), nil
	})
	ctx := context.Background()
	if _, e, err := c.SetReferralCandidate(ctx, "Ab12", key); err != nil || e != nil {
		t.Fatal(err, e)
	}
	if _, e, err := c.ClearReferralCandidate(ctx, key); err != nil || e != nil {
		t.Fatal(err, e)
	}
	if _, e, err := c.GetReferralInfo(ctx, referralTestID); err != nil || e != nil {
		t.Fatal(err, e)
	}
	if _, e, err := c.RequestRegistrationLink(ctx); err != nil || e != nil {
		t.Fatal(err, e)
	}
	if _, e, err := c.RequestRegistrationLinkWithCandidate(ctx, RegistrationReferralCandidate{referralTestID}); err != nil || e != nil {
		t.Fatal(err, e)
	}
	if n != len(steps) {
		t.Fatal(n)
	}
	// Invalid local actions must not reach the transport.
	if _, _, err := c.SetReferralCandidate(ctx, "bad code", key); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}
	if _, _, err := c.ClearReferralCandidate(ctx, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}
	if _, _, err := c.RequestRegistrationLinkWithCandidate(ctx, RegistrationReferralCandidate{"bad"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}
	c.HTTP = referralDoer(func(r *http.Request) (*http.Response, error) { return referralReply(referralInfoFixture, 200), nil })
	if _, _, err := c.GetReferralInfo(ctx, "aaaaaaaa-1234-1234-1234-123456789abc"); err == nil {
		t.Fatal("foreign account accepted")
	}
	c.HTTP = referralDoer(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := c.GetReferralInfo(canceled, referralTestID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestReferralErrorAndStatePreservation(t *testing.T) {
	c := Client{BaseURL: "https://service.invalid/api/mobile/v1"}
	c.HTTP = referralDoer(func(r *http.Request) (*http.Response, error) {
		return referralReply(`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-10-03T00:00:00Z","schema_version":"1.0","status":"error","code":"REFERRAL_HISTORY_PENDING","retryable":true}`, 503), nil
	})
	v, e, err := c.GetReferralInfo(context.Background(), referralTestID)
	if err != nil || e == nil || e.Code != "REFERRAL_HISTORY_PENDING" || v.Referral.Code != "" {
		t.Fatalf("history failure must not default eligible: %v %v", err, e)
	}
	c.HTTP = referralDoer(func(r *http.Request) (*http.Response, error) { return referralReply(referralCleared, 200), nil })
	if _, _, err := c.SetReferralCandidate(context.Background(), "Ab12", "referral-key-0001"); err == nil {
		t.Fatal("POST accepted cleared")
	}
}

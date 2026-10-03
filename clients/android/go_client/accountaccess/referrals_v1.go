package accountaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"wg-turn-client/wlwire"
)

const ReferralTermsVersion = "referral-20261003-v1"
const referralCandidatePath = "/referral/candidate"

var referralCodePattern = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

// ReferralCandidate is an installation-scoped candidate, never proof of account ownership.
type ReferralCandidate struct {
	ID    string `json:"id,omitempty"`
	State string `json:"state"`
	Code  string `json:"code,omitempty"`
}
type ReferralCandidateResponse struct {
	RequestID string            `json:"request_id"`
	Candidate ReferralCandidate `json:"candidate"`
}
type ReferralLinks struct {
	Telegram string `json:"telegram"`
	Web      string `json:"web"`
}
type ReferralAttribution struct {
	State     string  `json:"state"`
	ReceiptID *string `json:"receipt_id"`
	Reason    *string `json:"reason"`
}
type ReferralDiscount struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	State       string `json:"state"`
}
type ReferralBenefits struct {
	TrialBonusDays int64            `json:"trial_bonus_days"`
	Discount       ReferralDiscount `json:"discount"`
}
type ReferralRewards struct {
	WaitingDays int64 `json:"waiting_days"`
	AppliedDays int64 `json:"applied_days"`
}
type ReferralInfo struct {
	AccountRef   string              `json:"account_ref"`
	Code         string              `json:"code"`
	Links        ReferralLinks       `json:"links"`
	Attribution  ReferralAttribution `json:"attribution"`
	Benefits     ReferralBenefits    `json:"benefits"`
	Rewards      ReferralRewards     `json:"rewards"`
	TermsVersion string              `json:"terms_version"`
}
type ReferralInfoResponse struct {
	RequestID string       `json:"request_id"`
	Referral  ReferralInfo `json:"referral"`
}

// Closed referral objects require every exact key. Only attribution receipt/reason
// permit null. Reuse wlwire duplicate/trailing/unknown rejection and typed integers.
func referralObject(raw []byte, out any, keys ...string) error {
	if err := wlwire.StrictJSON(raw, out); err != nil {
		return fmt.Errorf("referral object invalid")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(keys) {
		return fmt.Errorf("referral fields invalid")
	}
	for _, k := range keys {
		v, ok := fields[k]
		if !ok || (string(v) == "null" && k != "receipt_id" && k != "reason") {
			return fmt.Errorf("referral field missing or null")
		}
	}
	return nil
}
func (v *ReferralCandidate) UnmarshalJSON(raw []byte) error {
	type plain ReferralCandidate
	var p plain
	if err := wlwire.StrictJSON(raw, &p); err != nil {
		return fmt.Errorf("referral candidate invalid")
	}
	switch p.State {
	case "pending":
		if err := referralObject(raw, &p, "id", "state", "code"); err != nil {
			return err
		}
		if !validUUID(p.ID) || !referralCodePattern.MatchString(p.Code) {
			return fmt.Errorf("referral candidate invalid")
		}
	case "cleared":
		if err := referralObject(raw, &p, "state"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("referral candidate state invalid")
	}
	*v = ReferralCandidate(p)
	return nil
}
func DecodeReferralCandidateStrict(raw []byte) (ReferralCandidateResponse, error) {
	var v ReferralCandidateResponse
	if err := referralObject(raw, &v, "request_id", "candidate"); err != nil {
		return ReferralCandidateResponse{}, err
	}
	if !requestIDPattern.MatchString(v.RequestID) {
		return ReferralCandidateResponse{}, fmt.Errorf("referral request id invalid")
	}
	return v, nil
}
func DecodeReferralInfoStrict(raw []byte) (ReferralInfoResponse, error) {
	var v ReferralInfoResponse
	if err := referralObject(raw, &v, "request_id", "referral"); err != nil {
		return ReferralInfoResponse{}, err
	}
	r := v.Referral
	if !requestIDPattern.MatchString(v.RequestID) || !validUUID(r.AccountRef) || !referralCodePattern.MatchString(r.Code) || r.TermsVersion != ReferralTermsVersion {
		return ReferralInfoResponse{}, fmt.Errorf("referral identity or terms invalid")
	}
	if r.Links.Telegram != "https://t.me/terlimo_vpn_wdtt_bot?start=ref_u"+r.Code || r.Links.Web != "https://terlimo.xyz/?ref=u"+r.Code {
		return ReferralInfoResponse{}, fmt.Errorf("referral links mismatch")
	}
	a := r.Attribution
	if a.ReceiptID != nil && !validUUID(*a.ReceiptID) {
		return ReferralInfoResponse{}, fmt.Errorf("referral receipt invalid")
	}
	switch a.State {
	case "none":
		if a.ReceiptID != nil || a.Reason != nil {
			return ReferralInfoResponse{}, fmt.Errorf("referral none inconsistent")
		}
	case "attached":
		if a.ReceiptID == nil || a.Reason != nil {
			return ReferralInfoResponse{}, fmt.Errorf("referral attached inconsistent")
		}
	case "rejected":
		if a.ReceiptID == nil || a.Reason == nil {
			return ReferralInfoResponse{}, fmt.Errorf("referral rejection missing")
		}
		switch *a.Reason {
		case "self", "already_attributed", "ineligible", "invalid":
		default:
			return ReferralInfoResponse{}, fmt.Errorf("referral reason invalid")
		}
	default:
		return ReferralInfoResponse{}, fmt.Errorf("referral attribution invalid")
	}
	d := r.Benefits.Discount
	if r.Benefits.TrialBonusDays < 0 || d.AmountMinor < 0 || d.Currency != "RUB" || r.Rewards.WaitingDays < 0 || r.Rewards.AppliedDays < 0 {
		return ReferralInfoResponse{}, fmt.Errorf("referral benefit invalid")
	}
	switch d.State {
	case "eligible", "reserved", "consumed", "ineligible", "history_pending":
	default:
		return ReferralInfoResponse{}, fmt.Errorf("referral discount state invalid")
	}
	return v, nil
}

// SetReferralCandidate preserves caller spelling and K. Server alone resolves legacy
// normalization/ownership. No retries, persistence or automatic registration here.
func (c *Client) SetReferralCandidate(ctx context.Context, code, key string) (ReferralCandidateResponse, *ErrorResponse, error) {
	if !referralCodePattern.MatchString(code) || !validIdempotencyKey(key) {
		return ReferralCandidateResponse{}, nil, ErrInvalidRequest
	}
	return c.referralCandidate(ctx, http.MethodPost, map[string]string{"code": code}, key, "pending")
}
func (c *Client) ClearReferralCandidate(ctx context.Context, key string) (ReferralCandidateResponse, *ErrorResponse, error) {
	if !validIdempotencyKey(key) {
		return ReferralCandidateResponse{}, nil, ErrInvalidRequest
	}
	return c.referralCandidate(ctx, http.MethodDelete, nil, key, "cleared")
}
func (c *Client) referralCandidate(ctx context.Context, method string, body any, key, state string) (ReferralCandidateResponse, *ErrorResponse, error) {
	raw, status, err := c.request(ctx, method, referralCandidatePath, body, key)
	if err != nil {
		return ReferralCandidateResponse{}, nil, err
	}
	if status != http.StatusOK {
		e, err := decodeError(raw, status)
		return ReferralCandidateResponse{}, e, err
	}
	v, err := DecodeReferralCandidateStrict(raw)
	if err == nil && v.Candidate.State != state {
		err = fmt.Errorf("referral response state mismatch")
	}
	if err != nil {
		return ReferralCandidateResponse{}, nil, err
	}
	return v, nil, nil
}

// expectedAccountRef is the caller's authenticated account fence. A syntactically
// valid foreign account response is never returned as this account's benefits.
func (c *Client) GetReferralInfo(ctx context.Context, expectedAccountRef string) (ReferralInfoResponse, *ErrorResponse, error) {
	if !validUUID(expectedAccountRef) {
		return ReferralInfoResponse{}, nil, ErrInvalidRequest
	}
	raw, status, err := c.request(ctx, http.MethodGet, "/referral", nil, "")
	if err != nil {
		return ReferralInfoResponse{}, nil, err
	}
	if status != http.StatusOK {
		e, err := decodeError(raw, status)
		return ReferralInfoResponse{}, e, err
	}
	v, err := DecodeReferralInfoStrict(raw)
	if err == nil && v.Referral.AccountRef != expectedAccountRef {
		err = fmt.Errorf("referral account mismatch")
	}
	if err != nil {
		return ReferralInfoResponse{}, nil, err
	}
	return v, nil, nil
}

// RegistrationReferralCandidate opts in explicitly. The server authenticates its
// installation owner; UUID syntax alone is not ownership evidence.
type RegistrationReferralCandidate struct {
	CandidateID string `json:"referral_candidate_id"`
}

func (c *Client) RequestRegistrationLinkWithCandidate(ctx context.Context, candidate RegistrationReferralCandidate) (RegistrationLink, *ErrorResponse, error) {
	if !validUUID(candidate.CandidateID) {
		return RegistrationLink{}, nil, ErrInvalidRequest
	}
	return c.requestRegistrationLink(ctx, candidate)
}

func (v *ReferralLinks) UnmarshalJSON(raw []byte) error {
	type plain ReferralLinks
	var p plain
	if err := referralObject(raw, &p, "telegram", "web"); err != nil {
		return err
	}
	*v = ReferralLinks(p)
	return nil
}

func (v *ReferralAttribution) UnmarshalJSON(raw []byte) error {
	type plain ReferralAttribution
	var p plain
	if err := referralObject(raw, &p, "state", "receipt_id", "reason"); err != nil {
		return err
	}
	*v = ReferralAttribution(p)
	return nil
}

func (v *ReferralDiscount) UnmarshalJSON(raw []byte) error {
	type plain ReferralDiscount
	var p plain
	if err := referralObject(raw, &p, "currency", "amount_minor", "state"); err != nil {
		return err
	}
	*v = ReferralDiscount(p)
	return nil
}

func (v *ReferralBenefits) UnmarshalJSON(raw []byte) error {
	type plain ReferralBenefits
	var p plain
	if err := referralObject(raw, &p, "trial_bonus_days", "discount"); err != nil {
		return err
	}
	*v = ReferralBenefits(p)
	return nil
}

func (v *ReferralRewards) UnmarshalJSON(raw []byte) error {
	type plain ReferralRewards
	var p plain
	if err := referralObject(raw, &p, "waiting_days", "applied_days"); err != nil {
		return err
	}
	*v = ReferralRewards(p)
	return nil
}

func (v *ReferralInfo) UnmarshalJSON(raw []byte) error {
	type plain ReferralInfo
	var p plain
	if err := referralObject(raw, &p, "account_ref", "code", "links", "attribution", "benefits", "rewards", "terms_version"); err != nil {
		return err
	}
	*v = ReferralInfo(p)
	return nil
}

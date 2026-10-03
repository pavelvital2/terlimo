// Package accountaccess implements the STEP03.4 native account-access coordinator:
// strict /me, /gateways, /access/sync and /operations/{id} contract consumption plus the
// account_access v1 native->host projection. Native is the sole fetch/retry/persist owner;
// the host only fences attempts/sessions and displays.
//
// Contract basis: accepted pair terlimo-s1-contracts 253cd8a2 (tree 18f2454a) and
// backend 35ff3194, shared decisions C01_SHARED_DECISIONS and the accepted
// generation/lease_seq delta. No live endpoint or credential is embedded here.
package accountaccess

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
	"wg-turn-client/wlwire"
)

const (
	SchemaVersion = "1.0"

	CodeAccessSyncPending   = "ACCESS_SYNC_PENDING"
	CodeServiceUnavailable  = "SERVICE_UNAVAILABLE"
	CodeRevisionConflict    = "REVISION_CONFLICT"
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	CodeSessionExpired      = "SESSION_EXPIRED"
	CodeSessionInvalid      = "SESSION_INVALID"
)

var (
	revisionPattern   = regexp.MustCompile(`^(0|[1-9][0-9]{0,18})$`)
	generationPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	utcTimePattern    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$`)
	requestIDPattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ValidRevision reports whether s is a contract decimal Revision.
func ValidRevision(s string) bool { return revisionPattern.MatchString(s) }

// ValidGeneration reports whether s is a contract decimal Generation (>= 1).
func ValidGeneration(s string) bool { return generationPattern.MatchString(s) }

// ValidUtcTime reports whether s is a contract UtcTime (UTC "Z" only, seconds or finer).
func ValidUtcTime(s string) bool { return utcTimePattern.MatchString(s) }

// CompareRevision compares two decimal Revision strings numerically without
// float conversion. It returns -1, 0 or 1 and ok=false for malformed input.
// The pattern allows up to 19 digits, so int64 is not wide enough.
func CompareRevision(a, b string) (int, bool) {
	if !ValidRevision(a) || !ValidRevision(b) {
		return 0, false
	}
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1, true
		}
		return 1, true
	}
	return strings.Compare(a, b), true
}

func decodeStrict(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("trailing JSON after contract message")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return fmt.Errorf("trailing JSON after contract message")
	}
	return nil
}

// meEntitlementPlan is the optional additive active-plan identity frozen for the
// entitlement. Absent/null means the plan is unknown and the client must not invent a
// name; it is never derived from plans/quote/source_ref/catalog.
//
// The object must carry exactly {id, title, duration_code}: the `title` key is mandatory
// and may be JSON string or JSON null. A missing title key is malformed, never "unknown"
// (only the whole plan being absent/null is unknown).
type meEntitlementPlan struct {
	ID           string
	Title        *string
	DurationCode string
}

func (p *meEntitlementPlan) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("entitlement plan must be an object")
	}
	if len(fields) != 3 {
		return fmt.Errorf("entitlement plan fields invalid")
	}
	idRaw, ok := fields["id"]
	if !ok {
		return fmt.Errorf("entitlement plan id missing")
	}
	titleRaw, ok := fields["title"]
	if !ok {
		return fmt.Errorf("entitlement plan title missing")
	}
	durationRaw, ok := fields["duration_code"]
	if !ok {
		return fmt.Errorf("entitlement plan duration_code missing")
	}
	if err := json.Unmarshal(idRaw, &p.ID); err != nil {
		return fmt.Errorf("entitlement plan id invalid")
	}
	if err := json.Unmarshal(durationRaw, &p.DurationCode); err != nil {
		return fmt.Errorf("entitlement plan duration_code invalid")
	}
	if string(titleRaw) == "null" {
		p.Title = nil
	} else {
		var title string
		if err := json.Unmarshal(titleRaw, &title); err != nil {
			return fmt.Errorf("entitlement plan title invalid")
		}
		p.Title = &title
	}
	return nil
}

type meEntitlement struct {
	Type                 string             `json:"type"`
	Status               string             `json:"status"`
	ValidFrom            *string            `json:"valid_from"`
	ValidUntil           *string            `json:"valid_until"`
	EffectiveDeviceLimit int                `json:"effective_device_limit"`
	SlotsUsed            int                `json:"slots_used"`
	Revision             string             `json:"revision"`
	PerpetualCommercial  bool               `json:"perpetual_commercial"`
	SourceRef            *string            `json:"source_ref"`
	Plan                 *meEntitlementPlan `json:"plan,omitempty"`
}

// Capacity is a nonnegative server/Android Int32 count, without a commercial cap.
// Required numbers cannot silently become zero through a missing or null field.
func (e *meEntitlement) UnmarshalJSON(raw []byte) error {
	type plain meEntitlement
	var value plain
	if err := unmarshalStrictRequired(raw, &value, "effective_device_limit", "slots_used"); err != nil {
		return err
	}
	if err := requiredNonNull(raw, "effective_device_limit", "slots_used"); err != nil {
		return err
	}
	*e = meEntitlement(value)
	return nil
}

type onboardingHour struct {
	State                string  `json:"state"`
	StartedBy            string  `json:"started_by"`
	StartedAt            *string `json:"started_at"`
	NotAfter             *string `json:"not_after"`
	DurationSeconds      int     `json:"duration_seconds"`
	OneTime              bool    `json:"one_time"`
	ExtendsOnRefresh     bool    `json:"extends_on_refresh"`
	ExtendsOnRestart     bool    `json:"extends_on_restart"`
	CreatesTrial         bool    `json:"creates_trial"`
	RequiresHardwareID   bool    `json:"requires_hardware_id"`
	Unit                 string  `json:"unit"`
	PostTelegramIdentity string  `json:"post_telegram_identity"`
	PreTelegramReinstall string  `json:"pre_telegram_reinstall"`
	// Revision is the server onboarding block revision; accepted as a JSON number.
	Revision json.Number `json:"revision"`
}

type grantResolution struct {
	ControlAvailable            bool    `json:"control_available"`
	RestrictedCheckoutAvailable bool    `json:"restricted_checkout_available"`
	DataAccess                  string  `json:"data_access"`
	EffectiveDeadline           *string `json:"effective_deadline"`
}

// meRegistration is the additive /me registration block (S3-A). It is server-owned state:
// the client never derives eligibility and never persists it as truth. An absent block is
// the pre-registration server and decodes to the zero value ("none").
type meRegistration struct {
	State             string  `json:"state"`
	TelegramID        *int64  `json:"telegram_id"`
	RegisteredAt      *string `json:"registered_at"`
	WithinHour        bool    `json:"within_hour"`
	TrialAvailable    bool    `json:"trial_available"`
	TrialReason       *string `json:"trial_reason"`
	PurchaseAvailable bool    `json:"purchase_available"`
	// LinkExpiresAt is present only while a pending registration link exists; it is
	// additive and display-only (no eligibility).
	LinkExpiresAt *string `json:"link_expires_at"`
}

// meTrial is the additive /me trial block (S3-B). Server truth only: the client shows the
// explicit activation action exactly when CanActivate is true and never derives eligibility.
// An absent block is the S3-A backend (no trial slice yet).
type meTrial struct {
	State       string  `json:"state"`
	CanActivate bool    `json:"can_activate"`
	Reason      *string `json:"reason"`
	StartsAt    *string `json:"starts_at"`
	EndsAt      *string `json:"ends_at"`
	// Replay is the additive server-owned flag present in /me.trial; display-only.
	Replay bool `json:"replay"`
}

// MeResponse is the flat GET /me projection consumed by the native coordinator.
// binding_revision is the real binding generation fence or null; it is never 0.
type MeResponse struct {
	RequestID       string          `json:"request_id"`
	ServerTime      string          `json:"server_time"`
	SchemaVersion   string          `json:"schema_version"`
	Status          string          `json:"status"`
	AccountState    string          `json:"account_state"`
	TelegramLinked  bool            `json:"telegram_linked"`
	Entitlement     meEntitlement   `json:"entitlement"`
	BindingStatus   string          `json:"binding_status"`
	BindingRevision *string         `json:"binding_revision"`
	ManagementOnly  bool            `json:"management_only"`
	AccountRef      *string         `json:"account_ref"`
	Onboarding      onboardingHour  `json:"onboarding"`
	GrantResolution grantResolution `json:"grant_resolution"`
	Registration    meRegistration  `json:"registration"`
	Trial           meTrial         `json:"trial"`
	Revision        string          `json:"revision"`
}

// UsageBucket is one account-traffic window of the /usage DTO. complete=false means the
// server has not yet collected the full window; the client must show that honestly.
type UsageBucket struct {
	Period   string `json:"period"`
	RXBytes  int64  `json:"rx_bytes"`
	TXBytes  int64  `json:"tx_bytes"`
	Complete bool   `json:"complete"`
}

// UsageResponse is a verified, received GET /usage 200 body. as_of is null when no
// trustworthy sample exists; coverage_start is null when no trustworthy collection history
// exists. Neither is backfilled by guess (a null as_of is never replaced by server_time).
type UsageResponse struct {
	RequestID     string        `json:"request_id"`
	ServerTime    string        `json:"server_time"`
	SchemaVersion string        `json:"schema_version"`
	Status        string        `json:"status"`
	Timezone      string        `json:"timezone"`
	AsOf          *string       `json:"as_of"`
	CoverageStart *string       `json:"coverage_start"`
	Buckets       []UsageBucket `json:"buckets"`
}

// DecodeUsageStrict validates the /usage payload against the frozen contract subset:
// exactly the three known periods, non-negative counters, no unknown fields.
func DecodeUsageStrict(raw []byte) (UsageResponse, error) {
	var usage UsageResponse
	if err := decodeStrict(raw, &usage); err != nil {
		return UsageResponse{}, fmt.Errorf("usage decode: %w", err)
	}
	if usage.SchemaVersion != SchemaVersion || usage.Status != "ok" {
		return UsageResponse{}, fmt.Errorf("usage envelope invalid")
	}
	if !requestIDPattern.MatchString(usage.RequestID) || !ValidUtcTime(usage.ServerTime) {
		return UsageResponse{}, fmt.Errorf("usage envelope fields invalid")
	}
	if usage.Timezone != "Europe/Moscow" {
		return UsageResponse{}, fmt.Errorf("usage timezone invalid")
	}
	if usage.AsOf != nil && !ValidUtcTime(*usage.AsOf) {
		return UsageResponse{}, fmt.Errorf("usage as_of invalid")
	}
	if usage.CoverageStart != nil && !ValidUtcTime(*usage.CoverageStart) {
		return UsageResponse{}, fmt.Errorf("usage coverage_start invalid")
	}
	if len(usage.Buckets) != 3 {
		return UsageResponse{}, fmt.Errorf("usage buckets invalid")
	}
	seen := map[string]bool{}
	for _, bucket := range usage.Buckets {
		switch bucket.Period {
		case "today", "7d", "30d":
		default:
			return UsageResponse{}, fmt.Errorf("usage bucket period invalid")
		}
		if seen[bucket.Period] {
			return UsageResponse{}, fmt.Errorf("usage bucket duplicate")
		}
		seen[bucket.Period] = true
		if bucket.RXBytes < 0 || bucket.TXBytes < 0 {
			return UsageResponse{}, fmt.Errorf("usage bucket negative")
		}
	}
	return usage, nil
}

// IndefiniteSubscriptionData reports whether a subscription_data grant may omit the
// finite effective_deadline: only a perpetual commercial entitlement that carries no
// valid_until. Indefinite never means unbounded: the verified catalog validity and the
// granted node lease stay the finite bounds of admission, and any other null deadline
// remains a malformed contract.
func IndefiniteSubscriptionData(me MeResponse) bool {
	return me.GrantResolution.DataAccess == "subscription_data" &&
		me.Entitlement.PerpetualCommercial &&
		me.Entitlement.ValidUntil == nil
}

// DecodeMeStrict validates the /me payload against the frozen contract subset.
func DecodeMeStrict(raw []byte) (MeResponse, error) {
	var me MeResponse
	if err := decodeStrict(raw, &me); err != nil {
		return MeResponse{}, fmt.Errorf("me decode: %w", err)
	}
	if me.SchemaVersion != SchemaVersion || me.Status != "ok" {
		return MeResponse{}, fmt.Errorf("me envelope invalid")
	}
	if !requestIDPattern.MatchString(me.RequestID) || !ValidUtcTime(me.ServerTime) {
		return MeResponse{}, fmt.Errorf("me envelope fields invalid")
	}
	if !ValidRevision(me.Revision) {
		return MeResponse{}, fmt.Errorf("me revision invalid")
	}
	if me.Entitlement.Revision == "" || !ValidRevision(me.Entitlement.Revision) {
		return MeResponse{}, fmt.Errorf("entitlement revision invalid")
	}
	switch me.AccountState {
	case "UNLINKED", "VERIFIED_NO_ENTITLEMENT", "VERIFIED_NO_SLOT", "ACTIVE_TRIAL", "ACTIVE_PAID", "EXPIRED", "REVOKED_SESSION":
	default:
		return MeResponse{}, fmt.Errorf("account_state enum invalid")
	}
	switch me.BindingStatus {
	case "none", "pending", "active", "revoked", "deactivated":
	default:
		return MeResponse{}, fmt.Errorf("binding_status enum invalid")
	}
	switch me.Entitlement.Type {
	case "none", "trial", "paid", "imported":
	default:
		return MeResponse{}, fmt.Errorf("entitlement type enum invalid")
	}
	switch me.Entitlement.Status {
	case "none", "active", "expired", "revoked", "unknown_review":
	default:
		return MeResponse{}, fmt.Errorf("entitlement status enum invalid")
	}
	if me.Entitlement.EffectiveDeviceLimit < 0 || me.Entitlement.EffectiveDeviceLimit > 1<<31-1 ||
		me.Entitlement.SlotsUsed < 0 || me.Entitlement.SlotsUsed > 1<<31-1 {
		return MeResponse{}, fmt.Errorf("entitlement limits out of range")
	}
	if me.Entitlement.ValidFrom != nil && !ValidUtcTime(*me.Entitlement.ValidFrom) {
		return MeResponse{}, fmt.Errorf("entitlement valid_from invalid")
	}
	if me.Entitlement.ValidUntil != nil && !ValidUtcTime(*me.Entitlement.ValidUntil) {
		return MeResponse{}, fmt.Errorf("entitlement valid_until invalid")
	}
	if me.Entitlement.SourceRef != nil && len(*me.Entitlement.SourceRef) > 128 {
		return MeResponse{}, fmt.Errorf("entitlement source_ref too long")
	}
	if plan := me.Entitlement.Plan; plan != nil {
		if plan.ID == "" || len(plan.ID) > 128 {
			return MeResponse{}, fmt.Errorf("entitlement plan id invalid")
		}
		if plan.Title != nil && len(*plan.Title) > 128 {
			return MeResponse{}, fmt.Errorf("entitlement plan title too long")
		}
		if plan.DurationCode == "" || len(plan.DurationCode) > 64 {
			return MeResponse{}, fmt.Errorf("entitlement plan duration_code invalid")
		}
	}
	if me.BindingRevision != nil {
		// The source is the real binding generation: never 0 and never the subject revision.
		if !generationPattern.MatchString(*me.BindingRevision) {
			return MeResponse{}, fmt.Errorf("binding_revision must be a non-zero decimal generation or null")
		}
	}
	if me.ManagementOnly && me.GrantResolution.DataAccess != "none" {
		return MeResponse{}, fmt.Errorf("management-only session must not carry data grants")
	}
	if me.Onboarding.DurationSeconds != 3600 || !me.Onboarding.OneTime ||
		me.Onboarding.ExtendsOnRefresh || me.Onboarding.ExtendsOnRestart ||
		me.Onboarding.CreatesTrial || me.Onboarding.RequiresHardwareID ||
		me.Onboarding.StartedBy != "server_confirmed_first_connection" ||
		me.Onboarding.Unit != "installation_fingerprint" {
		return MeResponse{}, fmt.Errorf("onboarding hour invariants violated")
	}
	switch me.Onboarding.State {
	case "not_started":
		if me.Onboarding.StartedAt != nil || me.Onboarding.NotAfter != nil {
			return MeResponse{}, fmt.Errorf("onboarding not_started must carry null instants")
		}
	case "active", "expired":
		if me.Onboarding.StartedAt == nil || me.Onboarding.NotAfter == nil ||
			!ValidUtcTime(*me.Onboarding.StartedAt) || !ValidUtcTime(*me.Onboarding.NotAfter) {
			return MeResponse{}, fmt.Errorf("onboarding instants invalid")
		}
	default:
		return MeResponse{}, fmt.Errorf("onboarding state unknown")
	}
	switch me.GrantResolution.DataAccess {
	case "onboarding_hour":
		if me.GrantResolution.EffectiveDeadline == nil || !ValidUtcTime(*me.GrantResolution.EffectiveDeadline) {
			return MeResponse{}, fmt.Errorf("data access requires a finite effective_deadline")
		}
	case "subscription_data":
		// The only allowed indefinite shape is the perpetual commercial entitlement
		// without valid_until; any other null deadline is still a malformed contract.
		if me.GrantResolution.EffectiveDeadline == nil {
			if !IndefiniteSubscriptionData(me) {
				return MeResponse{}, fmt.Errorf("data access requires a finite effective_deadline")
			}
		} else if !ValidUtcTime(*me.GrantResolution.EffectiveDeadline) {
			return MeResponse{}, fmt.Errorf("data access requires a finite effective_deadline")
		}
	case "none", "restricted_checkout":
		if me.GrantResolution.EffectiveDeadline != nil {
			return MeResponse{}, fmt.Errorf("no data access must not carry an effective_deadline")
		}
	default:
		return MeResponse{}, fmt.Errorf("data_access class unknown")
	}
	switch me.Registration.State {
	case "", "none", "pending", "registered":
	default:
		return MeResponse{}, fmt.Errorf("registration state unknown")
	}
	if me.Registration.RegisteredAt != nil && !ValidUtcTime(*me.Registration.RegisteredAt) {
		return MeResponse{}, fmt.Errorf("registration registered_at invalid")
	}
	if me.Registration.LinkExpiresAt != nil && !ValidUtcTime(*me.Registration.LinkExpiresAt) {
		return MeResponse{}, fmt.Errorf("registration link_expires_at invalid")
	}
	switch {
	case me.Registration.TrialReason == nil:
	case *me.Registration.TrialReason == "within_hour_no_prior_trial" ||
		*me.Registration.TrialReason == "hour_expired" ||
		*me.Registration.TrialReason == "trial_already_used":
	default:
		return MeResponse{}, fmt.Errorf("registration trial_reason unknown")
	}
	switch me.Trial.State {
	case "", "none", "available", "active", "used", "ineligible":
	default:
		return MeResponse{}, fmt.Errorf("trial state unknown")
	}
	if me.Trial.Reason != nil && len(*me.Trial.Reason) > 64 {
		return MeResponse{}, fmt.Errorf("trial reason too long")
	}
	if me.Trial.StartsAt != nil && !ValidUtcTime(*me.Trial.StartsAt) {
		return MeResponse{}, fmt.Errorf("trial starts_at invalid")
	}
	if me.Trial.EndsAt != nil && !ValidUtcTime(*me.Trial.EndsAt) {
		return MeResponse{}, fmt.Errorf("trial ends_at invalid")
	}
	if me.Trial.State == "active" && (me.Trial.StartsAt == nil || me.Trial.EndsAt == nil) {
		return MeResponse{}, fmt.Errorf("active trial requires both instants")
	}
	return me, nil
}

// RegistrationLink is the strict POST /registration/telegram/link projection. The raw
// one-time token and the deep link are carried only long enough to open Telegram; they
// are never persisted by the client.
type RegistrationLink struct {
	ReferralAttributionReceiptID *string
	State                        string
	Token                        string
	BotUsername                  string
	DeepLink                     string
	ExpiresAt                    *string
	ExpiresIn                    int
}

// DecodeRegistrationLinkStrict validates the link response against the frozen subset.
func DecodeRegistrationLinkStrict(raw []byte) (RegistrationLink, error) {
	var envelope struct {
		RequestID     string `json:"request_id"`
		ServerTime    string `json:"server_time"`
		SchemaVersion string `json:"schema_version"`
		Status        string `json:"status"`
		Registration  struct {
			ReferralAttributionReceiptID *string `json:"referral_attribution_receipt_id,omitempty"`
			State                        string  `json:"state"`
			Token                        *string `json:"token"`
			BotUsername                  *string `json:"bot_username"`
			DeepLink                     *string `json:"deep_link"`
			ExpiresAt                    *string `json:"expires_at"`
			ExpiresIn                    *int    `json:"expires_in"`
		} `json:"registration"`
	}
	if err := wlwire.StrictJSON(raw, &envelope); err != nil {
		return RegistrationLink{}, fmt.Errorf("registration link decode: %w", err)
	}
	if envelope.SchemaVersion != SchemaVersion || envelope.Status != "ok" ||
		!requestIDPattern.MatchString(envelope.RequestID) || !ValidUtcTime(envelope.ServerTime) {
		return RegistrationLink{}, fmt.Errorf("registration link envelope invalid")
	}
	receipt := envelope.Registration.ReferralAttributionReceiptID
	if receipt != nil && (!validUUID(*receipt) || envelope.Registration.State != "registered") {
		return RegistrationLink{}, fmt.Errorf("registration referral receipt invalid")
	}
	switch envelope.Registration.State {
	case "pending":
		if envelope.Registration.Token == nil || envelope.Registration.BotUsername == nil ||
			envelope.Registration.DeepLink == nil || envelope.Registration.ExpiresAt == nil ||
			envelope.Registration.ExpiresIn == nil {
			return RegistrationLink{}, fmt.Errorf("registration link pending fields missing")
		}
		token, bot := *envelope.Registration.Token, *envelope.Registration.BotUsername
		if token == "" || len(token) > 256 || bot == "" || len(bot) > 64 {
			return RegistrationLink{}, fmt.Errorf("registration link token invalid")
		}
		if *envelope.Registration.DeepLink != "https://t.me/"+bot+"?start="+token {
			return RegistrationLink{}, fmt.Errorf("registration deeplink mismatch")
		}
		if !ValidUtcTime(*envelope.Registration.ExpiresAt) ||
			*envelope.Registration.ExpiresIn < 1 || *envelope.Registration.ExpiresIn > 3600 {
			return RegistrationLink{}, fmt.Errorf("registration link expiry invalid")
		}
		return RegistrationLink{State: "pending", Token: token, BotUsername: bot,
			DeepLink: *envelope.Registration.DeepLink, ExpiresAt: envelope.Registration.ExpiresAt,
			ExpiresIn: *envelope.Registration.ExpiresIn}, nil
	case "registered":
		return RegistrationLink{State: "registered", ReferralAttributionReceiptID: receipt}, nil
	default:
		return RegistrationLink{}, fmt.Errorf("registration link state unknown")
	}
}

// TrialActivation is the strict POST /trial/activate projection. `replay=true` returns the
// original interval unchanged: the client shows the exhausted/used state and never offers a
// fresh issuance.
type TrialActivation struct {
	State        string
	Replay       bool
	StartsAt     string
	EndsAt       string
	Days         int
	AccountState string
}

// DecodeTrialActivationStrict validates the activation response against the frozen subset.
func DecodeTrialActivationStrict(raw []byte) (TrialActivation, error) {
	var envelope struct {
		RequestID     string `json:"request_id"`
		SchemaVersion string `json:"schema_version"`
		Status        string `json:"status"`
		Trial         struct {
			State        string `json:"state"`
			Replay       bool   `json:"replay"`
			StartsAt     string `json:"starts_at"`
			EndsAt       string `json:"ends_at"`
			Days         int    `json:"days"`
			AccountState string `json:"account_state"`
		} `json:"trial"`
	}
	if err := decodeStrict(raw, &envelope); err != nil {
		return TrialActivation{}, fmt.Errorf("trial activation decode: %w", err)
	}
	if envelope.SchemaVersion != SchemaVersion || envelope.Status != "ok" ||
		!requestIDPattern.MatchString(envelope.RequestID) {
		return TrialActivation{}, fmt.Errorf("trial activation envelope invalid")
	}
	if envelope.Trial.State != "active" || envelope.Trial.Days != 7 {
		return TrialActivation{}, fmt.Errorf("trial activation shape invalid")
	}
	if envelope.Trial.AccountState != "ACTIVE_TRIAL" {
		return TrialActivation{}, fmt.Errorf("trial activation account_state invalid")
	}
	if !ValidUtcTime(envelope.Trial.StartsAt) || !ValidUtcTime(envelope.Trial.EndsAt) ||
		envelope.Trial.StartsAt >= envelope.Trial.EndsAt {
		return TrialActivation{}, fmt.Errorf("trial activation instants invalid")
	}
	return TrialActivation{State: envelope.Trial.State, Replay: envelope.Trial.Replay,
		StartsAt: envelope.Trial.StartsAt, EndsAt: envelope.Trial.EndsAt,
		Days: envelope.Trial.Days, AccountState: envelope.Trial.AccountState}, nil
}

type transportDescriptor struct {
	Protocol       string `json:"protocol"`
	PeerIP         string `json:"peer_ip"`
	DTLSPort       int    `json:"dtls_port"`
	WGPort         int    `json:"wg_port"`
	DTLSSPKISHA256 string `json:"dtls_spki_sha256"`
}

type accessDescriptor struct {
	GrantID    string `json:"grant_id"`
	DeviceRef  string `json:"device_ref"`
	Password   string `json:"password"`
	Generation string `json:"generation"`
	NotAfter   string `json:"not_after"`
	// LeaseSeq is the confirmed applied vpn_auth Seq. The accepted contract makes it a
	// required Generation, so an absent or malformed value is a malformed catalog.
	LeaseSeq string   `json:"lease_seq"`
	VKHashes []string `json:"vk_hashes"`
}

type probeDescriptor struct {
	Kind      string  `json:"kind"`
	Host      string  `json:"host"`
	Port      int     `json:"port"`
	Path      *string `json:"path"`
	TimeoutMS *int    `json:"timeout_ms"`
}

type gateway struct {
	GatewayID     string              `json:"gateway_id"`
	Name          string              `json:"name"`
	Region        *string             `json:"region"`
	CountryCode   *string             `json:"country_code"`
	Capabilities  []string            `json:"capabilities"`
	TargetWorkers *int                `json:"target_workers"`
	Transport     transportDescriptor `json:"transport"`
	Probe         *probeDescriptor    `json:"probe"`
	Access        accessDescriptor    `json:"access"`
}

// CatalogResponse is a verified, received GET /gateways 200 body.
type CatalogResponse struct {
	RequestID     string    `json:"request_id"`
	ServerTime    string    `json:"server_time"`
	SchemaVersion string    `json:"schema_version"`
	Status        string    `json:"status"`
	Revision      string    `json:"revision"`
	ValidUntil    string    `json:"valid_until"`
	IssuedAt      string    `json:"issued_at"`
	Gateways      []gateway `json:"gateways"`
}

// DigestedCatalog returns the canonical digest of the protected catalog payload.
func (c CatalogResponse) DigestedCatalog() (string, error) {
	return canonicalDigest(map[string]any{
		"gateways":    c.Gateways,
		"revision":    c.Revision,
		"valid_until": c.ValidUntil,
	})
}

// DecodeCatalogStrict validates a /gateways 200 body. A malformed registered node is a
// whole-response failure, never a partial catalog.
func DecodeCatalogStrict(raw []byte) (CatalogResponse, error) {
	var catalog CatalogResponse
	if err := decodeStrict(raw, &catalog); err != nil {
		return CatalogResponse{}, fmt.Errorf("catalog decode: %w", err)
	}
	if catalog.SchemaVersion != SchemaVersion || catalog.Status != "ok" {
		return CatalogResponse{}, fmt.Errorf("catalog envelope invalid")
	}
	if !requestIDPattern.MatchString(catalog.RequestID) || !ValidUtcTime(catalog.ServerTime) ||
		!ValidUtcTime(catalog.ValidUntil) || !ValidUtcTime(catalog.IssuedAt) {
		return CatalogResponse{}, fmt.Errorf("catalog envelope fields invalid")
	}
	if !ValidRevision(catalog.Revision) {
		return CatalogResponse{}, fmt.Errorf("catalog revision invalid")
	}
	seen := map[string]bool{}
	for _, node := range catalog.Gateways {
		if node.GatewayID == "" || len(node.GatewayID) > 128 || seen[node.GatewayID] {
			return CatalogResponse{}, fmt.Errorf("gateway id invalid or duplicated")
		}
		seen[node.GatewayID] = true
		if node.Name == "" || len(node.Name) > 64 {
			return CatalogResponse{}, fmt.Errorf("gateway name invalid")
		}
		if node.Region != nil && len(*node.Region) > 64 {
			return CatalogResponse{}, fmt.Errorf("gateway region too long")
		}
		if node.CountryCode != nil && len(*node.CountryCode) > 8 {
			return CatalogResponse{}, fmt.Errorf("gateway country_code too long")
		}
		capabilities := map[string]bool{}
		for _, capability := range node.Capabilities {
			if len(capability) > 64 || capabilities[capability] {
				return CatalogResponse{}, fmt.Errorf("gateway capabilities invalid")
			}
			capabilities[capability] = true
		}
		if node.TargetWorkers != nil && (*node.TargetWorkers < 1 || *node.TargetWorkers > 1024) {
			return CatalogResponse{}, fmt.Errorf("gateway target_workers out of range")
		}
		if node.Transport.Protocol != "wdtt-v17" ||
			node.Transport.PeerIP == "" || len(node.Transport.PeerIP) > 253 ||
			node.Transport.DTLSPort < 1 || node.Transport.DTLSPort > 65535 ||
			node.Transport.WGPort < 1 || node.Transport.WGPort > 65535 {
			return CatalogResponse{}, fmt.Errorf("gateway transport descriptor invalid")
		}
		if decoded, err := decodeBase64URL32(node.Transport.DTLSSPKISHA256); err != nil || decoded == "" {
			return CatalogResponse{}, fmt.Errorf("gateway spki invalid")
		}
		if node.Probe != nil {
			if node.Probe.Kind != "tcp_connect" && node.Probe.Kind != "https_head" {
				return CatalogResponse{}, fmt.Errorf("gateway probe kind invalid")
			}
			if node.Probe.Host == "" || len(node.Probe.Host) > 253 ||
				node.Probe.Port < 1 || node.Probe.Port > 65535 {
				return CatalogResponse{}, fmt.Errorf("gateway probe endpoint invalid")
			}
			if node.Probe.Path != nil && len(*node.Probe.Path) > 256 {
				return CatalogResponse{}, fmt.Errorf("gateway probe path too long")
			}
			if node.Probe.TimeoutMS != nil && (*node.Probe.TimeoutMS < 1 || *node.Probe.TimeoutMS > 60_000) {
				return CatalogResponse{}, fmt.Errorf("gateway probe timeout out of range")
			}
		}
		if node.Access.GrantID == "" || node.Access.DeviceRef == "" || node.Access.Password == "" ||
			!ValidGeneration(node.Access.Generation) || !ValidUtcTime(node.Access.NotAfter) {
			return CatalogResponse{}, fmt.Errorf("gateway access descriptor invalid")
		}
		if node.Access.LeaseSeq == "" || !ValidGeneration(node.Access.LeaseSeq) {
			return CatalogResponse{}, fmt.Errorf("gateway lease_seq required and must be a decimal generation")
		}
		if len(node.Access.VKHashes) > 8 {
			return CatalogResponse{}, fmt.Errorf("gateway vk_hashes too many")
		}
		for _, hash := range node.Access.VKHashes {
			if len(hash) > 128 {
				return CatalogResponse{}, fmt.Errorf("gateway vk_hash invalid")
			}
		}
	}
	return catalog, nil
}

// CatalogModeBrowse is the display-only catalog mode token. A browse response is not
// an admission snapshot: it carries no revision, no access/transport/probe and no
// secrets, and it may never replace the last-good verified catalog.
const CatalogModeBrowse = "browse"

// BrowseGateway is one display-only gateway row: the stable public catalog key plus
// display metadata. No access, transport or probe field can decode into it.
type BrowseGateway struct {
	GatewayID   string  `json:"gateway_id"`
	Name        string  `json:"name"`
	Region      *string `json:"region"`
	CountryCode *string `json:"country_code"`
}

// BrowseCatalogResponse is the display-only GET /gateways 200 body. The strict decoder
// rejects every credential/admission field, so a browse response can never smuggle a
// revision, lease, access descriptor or secret into the credential path.
type BrowseCatalogResponse struct {
	RequestID     string          `json:"request_id"`
	ServerTime    string          `json:"server_time"`
	SchemaVersion string          `json:"schema_version"`
	Status        string          `json:"status"`
	IssuedAt      string          `json:"issued_at"`
	ValidUntil    string          `json:"valid_until"`
	CatalogMode   string          `json:"catalog_mode"`
	Gateways      []BrowseGateway `json:"gateways"`
}

// Gateways is the strict union of the GET /gateways 200 bodies: exactly one of
// Catalog (credential-bearing admission catalog) and Browse (display-only metadata
// catalog) is non-nil.
type Gateways struct {
	Catalog *CatalogResponse
	Browse  *BrowseCatalogResponse
}

// gatewayModeProbe reads only the branch selector. It is intentionally not strict:
// each branch then strictly decodes the whole body, so a field of the other branch is
// an unknown-field failure and the union stays exact.
func gatewayModeProbe(raw []byte) (string, error) {
	var probe struct {
		CatalogMode *string `json:"catalog_mode"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", fmt.Errorf("gateways decode: %w", err)
	}
	if probe.CatalogMode == nil {
		return "", nil
	}
	return *probe.CatalogMode, nil
}

// DecodeGatewaysStrict splits the response BEFORE any credential decoding: an absent
// catalog_mode keeps the existing strict credential catalog exactly as before, the
// explicit "browse" token selects the display-only decoder, and any other token is
// malformed.
func DecodeGatewaysStrict(raw []byte) (Gateways, error) {
	mode, err := gatewayModeProbe(raw)
	if err != nil {
		return Gateways{}, err
	}
	switch mode {
	case "":
		catalog, err := DecodeCatalogStrict(raw)
		if err != nil {
			return Gateways{}, err
		}
		return Gateways{Catalog: &catalog}, nil
	case CatalogModeBrowse:
		browse, err := DecodeBrowseStrict(raw)
		if err != nil {
			return Gateways{}, err
		}
		return Gateways{Browse: &browse}, nil
	default:
		return Gateways{}, fmt.Errorf("catalog_mode %q unknown", mode)
	}
}

// DecodeBrowseStrict validates a display-only /gateways 200 body. The union is exact:
// revision, access, transport, probe or any other credential field is an unknown-field
// failure, and an empty gateway list is accepted only as a real successful response.
func DecodeBrowseStrict(raw []byte) (BrowseCatalogResponse, error) {
	var browse BrowseCatalogResponse
	if err := decodeStrict(raw, &browse); err != nil {
		return BrowseCatalogResponse{}, fmt.Errorf("browse catalog decode: %w", err)
	}
	if browse.SchemaVersion != SchemaVersion ||
		browse.Status != "ok" || browse.CatalogMode != CatalogModeBrowse {
		return BrowseCatalogResponse{}, fmt.Errorf("browse catalog envelope invalid")
	}
	if !requestIDPattern.MatchString(browse.RequestID) || !ValidUtcTime(browse.ServerTime) ||
		!ValidUtcTime(browse.ValidUntil) || !ValidUtcTime(browse.IssuedAt) {
		return BrowseCatalogResponse{}, fmt.Errorf("browse catalog envelope fields invalid")
	}
	seen := map[string]bool{}
	for _, node := range browse.Gateways {
		if node.GatewayID == "" || len(node.GatewayID) > 128 || seen[node.GatewayID] {
			return BrowseCatalogResponse{}, fmt.Errorf("browse gateway id invalid or duplicated")
		}
		seen[node.GatewayID] = true
		if node.Name == "" || len(node.Name) > 64 {
			return BrowseCatalogResponse{}, fmt.Errorf("browse gateway name invalid")
		}
		if node.Region != nil && len(*node.Region) > 64 {
			return BrowseCatalogResponse{}, fmt.Errorf("browse gateway region too long")
		}
		if node.CountryCode != nil && len(*node.CountryCode) > 8 {
			return BrowseCatalogResponse{}, fmt.Errorf("browse gateway country_code too long")
		}
	}
	return browse, nil
}

// Grant is one /access/sync per-node result.
type Grant struct {
	GrantID           string  `json:"grant_id"`
	BindingRef        string  `json:"binding_ref"`
	GatewayID         string  `json:"gateway_id"`
	DesiredGeneration string  `json:"desired_generation"`
	AppliedGeneration *string `json:"applied_generation"`
	NotAfter          string  `json:"not_after"`
	State             string  `json:"state"`
	LastError         *string `json:"last_error"`
}

// AccessSyncResponse mirrors the /access/sync 200 object.
type AccessSyncResponse struct {
	RequestID              string  `json:"request_id"`
	ServerTime             string  `json:"server_time"`
	SchemaVersion          string  `json:"schema_version"`
	Status                 string  `json:"status"`
	OperationID            string  `json:"operation_id"`
	AccessApplicationState string  `json:"access_application_state"`
	Revision               *string `json:"revision"`
	Grants                 []Grant `json:"grants"`
}

// Terminal reports whether the operation state is final.
func (r AccessSyncResponse) Terminal() bool {
	return r.AccessApplicationState == "applied" || r.AccessApplicationState == "rejected"
}

type nodeProgress struct {
	GatewayID         string  `json:"gateway_id"`
	State             string  `json:"state"`
	AppliedGeneration *string `json:"applied_generation"`
	NotAfter          *string `json:"not_after"`
	LastError         *string `json:"last_error"`
	Attempts          int     `json:"attempts"`
}

// OperationResponse mirrors GET /operations/{id}.
type OperationResponse struct {
	RequestID              string         `json:"request_id"`
	ServerTime             string         `json:"server_time"`
	SchemaVersion          string         `json:"schema_version"`
	Status                 string         `json:"status"`
	OperationID            string         `json:"operation_id"`
	OperationType          string         `json:"operation_type"`
	State                  string         `json:"state"`
	AccessApplicationState string         `json:"access_application_state"`
	PerNode                []nodeProgress `json:"per_node"`
	// SyncRecovery is the exact opt-in additive marker returned only for
	// GET /operations/{id}?observe=sync_recovery. Legacy responses without it decode
	// unchanged; unknown fields (including unknown nested fields) are still rejected
	// by decodeStrict.
	SyncRecovery *SyncRecovery `json:"sync_recovery,omitempty"`
}

// SyncRecovery is the exact observe=sync_recovery payload: the deployed server predicate
// plus the current subject revision pair. CatalogRevision/BindingRevision are contract
// decimals whenever a subject exists; Required is true only for the recoverable expired
// failed apply predicate, never for a blanket dead/exhausted rule.
type SyncRecovery struct {
	Required        bool   `json:"required"`
	CatalogRevision string `json:"catalog_revision"`
	BindingRevision string `json:"binding_revision"`
}

// ErrorResponse mirrors the bounded error envelope, including the GET /gateways
// nonterminal admission tokens.
type ErrorResponse struct {
	// Set only by the authenticated POST /payments response validator. Never decoded from JSON.
	expiredQuoteNoOrder bool
	RequestID           string         `json:"request_id"`
	ServerTime          string         `json:"server_time"`
	SchemaVersion       string         `json:"schema_version"`
	Status              string         `json:"status"`
	Code                string         `json:"code"`
	Retryable           bool           `json:"retryable"`
	RetryAfterMS        *int           `json:"retry_after_ms"`
	MessageKey          *string        `json:"message_key"`
	Details             map[string]any `json:"details"`
}

// DecodeErrorStrict validates an error envelope.
func DecodeErrorStrict(raw []byte) (ErrorResponse, error) {
	var envelope ErrorResponse
	if err := decodeStrict(raw, &envelope); err != nil {
		return ErrorResponse{}, fmt.Errorf("error decode: %w", err)
	}
	if envelope.SchemaVersion != SchemaVersion || envelope.Status != "error" || envelope.Code == "" {
		return ErrorResponse{}, fmt.Errorf("error envelope invalid")
	}
	if !ValidUtcTime(envelope.ServerTime) {
		return ErrorResponse{}, fmt.Errorf("error server_time invalid")
	}
	if envelope.RetryAfterMS != nil && (*envelope.RetryAfterMS < 0 || *envelope.RetryAfterMS > 3_600_000) {
		return ErrorResponse{}, fmt.Errorf("retry_after_ms out of range")
	}
	return envelope, nil
}

// AdmissionTokens returns the bounded GET /gateways nonterminal admission metadata.
// These are admission tokens for a NEW sync only: they are not a catalog and never
// authorize clearing the last-good catalog.
func (e ErrorResponse) AdmissionTokens() (catalogRevision, bindingRevision string, ok bool) {
	catalogRevision, _ = e.Details["catalog_revision"].(string)
	bindingRevision, _ = e.Details["binding_revision"].(string)
	ok = ValidRevision(catalogRevision) && ValidRevision(bindingRevision)
	return catalogRevision, bindingRevision, ok
}

// canonicalDigest implements the accepted canonical JSON rules where the Go standard
// library allows: UTF-8, NFC strings, sorted keys, no insignificant whitespace.
func canonicalDigest(value any) (string, error) {
	canonical, err := canonicalJSON(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalJSON(value any) ([]byte, error) {
	switch typed := value.(type) {
	case nil:
		return []byte("null"), nil
	case bool:
		if typed {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case string:
		return json.Marshal(norm.NFC.String(typed))
	case int:
		return []byte(fmt.Sprintf("%d", typed)), nil
	case float64:
		return json.Marshal(typed)
	case *string:
		if typed == nil {
			return []byte("null"), nil
		}
		return json.Marshal(norm.NFC.String(*typed))
	case []string:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, item)
		}
		return canonicalJSON(items)
	case []any:
		var buffer bytes.Buffer
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			encoded, err := canonicalJSON(item)
			if err != nil {
				return nil, err
			}
			buffer.Write(encoded)
		}
		buffer.WriteByte(']')
		return buffer.Bytes(), nil
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var buffer bytes.Buffer
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			encodedKey, _ := json.Marshal(key)
			buffer.Write(encodedKey)
			buffer.WriteByte(':')
			encodedValue, err := canonicalJSON(typed[key])
			if err != nil {
				return nil, err
			}
			buffer.Write(encodedValue)
		}
		buffer.WriteByte('}')
		return buffer.Bytes(), nil
	}
	// Typed structs are normalized through JSON first; consumers of this package use
	// closed shapes, so the normalized generic form is deterministic.
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return canonicalJSON(generic)
}

func decodeBase64URL32(value string) (string, error) {
	if len(value) != 43 {
		return "", fmt.Errorf("spki length invalid")
	}
	raw, err := base64URLDecode(value)
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("spki not 32 bytes")
	}
	if base64URLEncode(raw) != value {
		return "", fmt.Errorf("spki not canonical base64url")
	}
	return value, nil
}

func parseUtc(value string) (time.Time, error) {
	if !ValidUtcTime(value) {
		return time.Time{}, fmt.Errorf("not a contract UtcTime: %q", value)
	}
	return time.Parse(time.RFC3339Nano, value)
}

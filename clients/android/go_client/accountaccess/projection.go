package accountaccess

// Projection is the account_access v1 native->host bridge payload. The envelope keys
// v/attempt_id are stamped by the managed bridge sender; Payload() emits exactly the
// frozen remaining key set and no additive fields.
type Projection struct {
	ServerTime      string
	Generation      uint64
	Previous        *uint64
	AccessRevision  string
	Account         map[string]any
	Entitlement     map[string]any
	Onboarding      map[string]any
	GrantResolution map[string]any
	// Registration is the additive S3-A display subset of /me.registration. It is carried
	// outside the protected access digest: registration is not an access right and never
	// changes the admission decision.
	Registration map[string]any
	// Trial is the additive S3-B display subset of /me.trial. Server truth only; never a
	// client-derived eligibility and never part of the protected access digest.
	Trial map[string]any
}

func (p Projection) account() map[string]any {
	out := map[string]any{
		"state":           p.Account["state"],
		"telegram_linked": p.Account["telegram_linked"],
		"binding_status":  p.Account["binding_status"],
		"management_only": p.Account["management_only"],
		"account_ref":     p.Account["account_ref"],
	}
	return out
}

func (p Projection) entitlementPayload() map[string]any { return p.Entitlement }
func (p Projection) onboardingPayload() map[string]any  { return p.Onboarding }
func (p Projection) grantPayload() map[string]any       { return p.GrantResolution }

// Protected is the canonical protected business payload used for same-revision
// duplicate/conflict comparisons (contract section 5.1).
func (p Projection) Protected() map[string]any {
	return map[string]any{
		"account":          p.account(),
		"entitlement":      p.entitlementPayload(),
		"onboarding":       p.onboardingPayload(),
		"grant_resolution": p.grantPayload(),
	}
}

// Digest is the canonical digest of the protected payload.
func (p Projection) Digest() (string, error) { return canonicalDigest(p.Protected()) }

// Payload returns the exact frozen key set without the bridge envelope keys.
func (p Projection) Payload() map[string]any {
	return map[string]any{
		"type":                        "account_access",
		"access_version":              1,
		"server_time":                 p.ServerTime,
		"session_generation":          revisionString(p.Generation),
		"previous_session_generation": nullableRevision(p.Previous),
		"access_revision":             p.AccessRevision,
		"account":                     p.account(),
		"entitlement":                 p.entitlementPayload(),
		"onboarding":                  p.onboardingPayload(),
		"grant_resolution":            p.grantPayload(),
		"registration":                p.registrationPayload(),
		"trial":                       p.trialPayload(),
	}
}

func (p Projection) trialPayload() map[string]any {
	out := map[string]any{
		"state":        "none",
		"can_activate": false,
		"reason":       nil,
		"starts_at":    nil,
		"ends_at":      nil,
	}
	for key, value := range p.Trial {
		out[key] = value
	}
	return out
}

func (p Projection) registrationPayload() map[string]any {
	out := map[string]any{
		"state":              "none",
		"within_hour":        false,
		"trial_available":    false,
		"trial_reason":       nil,
		"purchase_available": false,
	}
	for key, value := range p.Registration {
		out[key] = value
	}
	return out
}

// Envelope returns the complete frame including the envelope keys the schema requires.
func (p Projection) Envelope(attemptID string) map[string]any {
	out := p.Payload()
	out["v"] = 1
	out["attempt_id"] = attemptID
	return out
}

func revisionString(value uint64) string {
	if value == 0 {
		return "0"
	}
	buffer := [20]byte{}
	pos := len(buffer)
	for value > 0 {
		pos--
		buffer[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[pos:])
}

func nullableRevision(value *uint64) any {
	if value == nil {
		return nil
	}
	return revisionString(*value)
}

// buildProjection maps the flat /me response to the grouped bridge projection.
// Optional entitlement fields are present only when upstream carries them.
func buildProjection(me MeResponse, generation uint64, previous *uint64) Projection {
	entitlement := map[string]any{
		"type":                   me.Entitlement.Type,
		"status":                 me.Entitlement.Status,
		"effective_device_limit": me.Entitlement.EffectiveDeviceLimit,
		"slots_used":             me.Entitlement.SlotsUsed,
		"revision":               me.Entitlement.Revision,
		"perpetual_commercial":   me.Entitlement.PerpetualCommercial,
	}
	if me.Entitlement.ValidFrom != nil {
		entitlement["valid_from"] = *me.Entitlement.ValidFrom
	}
	if me.Entitlement.ValidUntil != nil {
		entitlement["valid_until"] = *me.Entitlement.ValidUntil
	}
	if me.Entitlement.SourceRef != nil {
		entitlement["source_ref"] = *me.Entitlement.SourceRef
	}
	if me.Entitlement.Plan != nil {
		// Additive active-plan identity only; title may be null and is forwarded verbatim.
		var title any
		if me.Entitlement.Plan.Title != nil {
			title = *me.Entitlement.Plan.Title
		}
		entitlement["plan"] = map[string]any{
			"id":            me.Entitlement.Plan.ID,
			"title":         title,
			"duration_code": me.Entitlement.Plan.DurationCode,
		}
	}
	onboarding := map[string]any{
		"state":                  me.Onboarding.State,
		"started_by":             me.Onboarding.StartedBy,
		"started_at":             nullableString(me.Onboarding.StartedAt),
		"not_after":              nullableString(me.Onboarding.NotAfter),
		"duration_seconds":       me.Onboarding.DurationSeconds,
		"one_time":               me.Onboarding.OneTime,
		"extends_on_refresh":     me.Onboarding.ExtendsOnRefresh,
		"extends_on_restart":     me.Onboarding.ExtendsOnRestart,
		"creates_trial":          me.Onboarding.CreatesTrial,
		"requires_hardware_id":   me.Onboarding.RequiresHardwareID,
		"unit":                   me.Onboarding.Unit,
		"post_telegram_identity": me.Onboarding.PostTelegramIdentity,
		"pre_telegram_reinstall": me.Onboarding.PreTelegramReinstall,
	}
	grant := map[string]any{
		"control_available":             me.GrantResolution.ControlAvailable,
		"restricted_checkout_available": me.GrantResolution.RestrictedCheckoutAvailable,
		"data_access":                   me.GrantResolution.DataAccess,
		"effective_deadline":            nullableString(me.GrantResolution.EffectiveDeadline),
	}
	registrationState := me.Registration.State
	if registrationState == "" {
		registrationState = "none"
	}
	registration := map[string]any{
		"state":              registrationState,
		"within_hour":        me.Registration.WithinHour,
		"trial_available":    me.Registration.TrialAvailable,
		"trial_reason":       nullableString(me.Registration.TrialReason),
		"purchase_available": me.Registration.PurchaseAvailable,
	}
	trialState := me.Trial.State
	if trialState == "" {
		trialState = "none"
	}
	trial := map[string]any{
		"state":        trialState,
		"can_activate": me.Trial.CanActivate,
		"reason":       nullableString(me.Trial.Reason),
		"starts_at":    nullableString(me.Trial.StartsAt),
		"ends_at":      nullableString(me.Trial.EndsAt),
	}
	return Projection{
		ServerTime:     me.ServerTime,
		Generation:     generation,
		Previous:       previous,
		AccessRevision: me.Revision,
		Account: map[string]any{
			"state":           me.AccountState,
			"telegram_linked": me.TelegramLinked,
			"binding_status":  me.BindingStatus,
			"management_only": me.ManagementOnly,
			"account_ref":     nullableString(me.AccountRef),
		},
		Entitlement:     entitlement,
		Onboarding:      onboarding,
		GrantResolution: grant,
		Registration:    registration,
		Trial:           trial,
	}
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

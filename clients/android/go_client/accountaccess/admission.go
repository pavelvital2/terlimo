package accountaccess

// Mobile admission: the Backend is the sole authority in mobile mode. The verified
// /me + /gateways pair decides whether a ready technical grant may feed the existing
// transport. This module never synthesizes legacy subscription/link business fields and
// never falls back to the legacy wlbs authorization.

import (
	"time"
)

// MobileGrant is the minimal technical admission handed to the existing transport.
// Field mapping (accepted): gateway_id = WDTT node_id, device_ref = RegistrationID =
// installation fingerprint; grant_id/password/generation/not_after from mobile catalog;
// lease_seq is the required vpn_auth Seq of the accepted contract.
type MobileGrant struct {
	NodeID         string
	GrantID        string
	RegistrationID string
	Password       string
	Generation     string
	LeaseSeq       string
	NotAfter       time.Time
	PeerIP         string
	DTLSPort       int
	WGPort         int
	DTLSSPKISHA256 string
	TargetWorkers  int
}

// AdmissionDecision is a pure, display-safe decision. StopDataPlane means an explicit
// revoke/expiry/removal is proven and the data plane must stop; the caller must not
// continue through the legacy path.
type AdmissionDecision struct {
	Admitted      bool
	StopDataPlane bool
	Reason        string
	Dependency    string
	Grant         *MobileGrant
}

// GrantFromGateway projects one verified catalog gateway into the technical grant.
// lease_seq is never defaulted: with the accepted contract the strict decoder already
// rejects an absent value, and a manually built response without it stays an explicit
// malformed-contract dependency instead of a silent zero.
func GrantFromGateway(catalog CatalogResponse, gatewayID string) AdmissionDecision {
	for _, node := range catalog.Gateways {
		if node.GatewayID != gatewayID {
			continue
		}
		notAfter, err := parseUtc(node.Access.NotAfter)
		if err != nil {
			return AdmissionDecision{Reason: "GRANT_INVALID"}
		}
		grant := &MobileGrant{
			NodeID:         node.GatewayID,
			GrantID:        node.Access.GrantID,
			RegistrationID: node.Access.DeviceRef,
			Password:       node.Access.Password,
			Generation:     node.Access.Generation,
			NotAfter:       notAfter,
			PeerIP:         node.Transport.PeerIP,
			DTLSPort:       node.Transport.DTLSPort,
			WGPort:         node.Transport.WGPort,
			DTLSSPKISHA256: node.Transport.DTLSSPKISHA256,
		}
		if node.TargetWorkers != nil {
			grant.TargetWorkers = *node.TargetWorkers
		}
		if node.Access.LeaseSeq == "" {
			return AdmissionDecision{Reason: "LEASE_SEQ_MISSING", Dependency: "access.lease_seq is required by the accepted contract", Grant: grant}
		}
		if !ValidGeneration(node.Access.LeaseSeq) {
			return AdmissionDecision{Reason: "LEASE_SEQ_INVALID"}
		}
		grant.LeaseSeq = node.Access.LeaseSeq
		return AdmissionDecision{Admitted: true, Reason: "ADMITTED", Grant: grant}
	}
	return AdmissionDecision{Reason: "SELECTED_NODE_REMOVED", StopDataPlane: true}
}

// DecideAdmission evaluates the verified subject/catalog pair for one selected gateway.
// Order: explicit revoke/expiry/state and the catalog validity fence first (stop), then
// grant readiness. A contradictory or stale snapshot is never a right.
// confirmedActiveOnboardingHour reports the only per-installation state that may take
// precedence over an expired commercial subscription: a server-confirmed, unexpired
// onboarding hour carried by the verified snapshot. It is never inferred from UI state
// or a local timer, and it grants nothing on its own (catalog and lease fences below
// still apply).
func confirmedActiveOnboardingHour(me MeResponse, now time.Time) bool {
	if me.GrantResolution.DataAccess != "onboarding_hour" || me.Onboarding.State != "active" ||
		me.GrantResolution.EffectiveDeadline == nil {
		return false
	}
	deadline, err := parseUtc(*me.GrantResolution.EffectiveDeadline)
	return err == nil && deadline.After(now)
}

func DecideAdmission(me MeResponse, catalog CatalogResponse, gatewayID string, now time.Time) AdmissionDecision {
	switch me.AccountState {
	case "EXPIRED":
		// The installation hour is independent of the commercial subscription: a
		// confirmed active hour outlives account expiry, while revoked sessions,
		// revoked bindings/entitlements and every other fence below still stop.
		if !confirmedActiveOnboardingHour(me, now) {
			return AdmissionDecision{StopDataPlane: true, Reason: "ACCOUNT_EXPIRED"}
		}
	case "REVOKED_SESSION":
		return AdmissionDecision{StopDataPlane: true, Reason: "SESSION_REVOKED"}
	}
	switch me.BindingStatus {
	case "revoked":
		return AdmissionDecision{StopDataPlane: true, Reason: "BINDING_REVOKED"}
	case "deactivated":
		return AdmissionDecision{StopDataPlane: true, Reason: "BINDING_DEACTIVATED"}
	}
	switch me.Entitlement.Status {
	case "revoked":
		return AdmissionDecision{StopDataPlane: true, Reason: "ENTITLEMENT_REVOKED"}
	case "unknown_review":
		return AdmissionDecision{StopDataPlane: true, Reason: "ENTITLEMENT_UNREVIEWED"}
	case "expired":
		return AdmissionDecision{StopDataPlane: true, Reason: "ENTITLEMENT_EXPIRED"}
	}
	validUntil, err := parseUtc(catalog.ValidUntil)
	if err != nil {
		return AdmissionDecision{StopDataPlane: true, Reason: "CATALOG_INVALID"}
	}
	if !validUntil.After(now) {
		// The declared verified catalog validity is the stop fence: refresh floor,
		// backoff or a pending network never postpone it.
		return AdmissionDecision{StopDataPlane: true, Reason: "CATALOG_EXPIRED"}
	}
	switch me.GrantResolution.DataAccess {
	case "none":
		return AdmissionDecision{StopDataPlane: true, Reason: "DATA_ACCESS_NONE"}
	case "restricted_checkout":
		return AdmissionDecision{StopDataPlane: true, Reason: "RESTRICTED_CHECKOUT"}
	case "onboarding_hour":
		// Onboarding is per installation: it never requires a paid subscription or an
		// active binding, but a revoked binding already stopped above.
	case "subscription_data":
		if me.Entitlement.Status != "active" || me.Entitlement.Type == "none" ||
			me.BindingStatus != "active" ||
			(me.AccountState != "ACTIVE_TRIAL" && me.AccountState != "ACTIVE_PAID") {
			return AdmissionDecision{StopDataPlane: true, Reason: "SUBSCRIPTION_NOT_ACTIVE"}
		}
	default:
		return AdmissionDecision{StopDataPlane: true, Reason: "DATA_ACCESS_UNKNOWN"}
	}
	if me.GrantResolution.EffectiveDeadline == nil {
		// The indefinite commercial subscription carries no entitlement deadline; its
		// finite bounds are the verified catalog validity above and the selected node
		// lease below. Every other null deadline stays fail-closed.
		if !IndefiniteSubscriptionData(me) {
			return AdmissionDecision{StopDataPlane: true, Reason: "RIGHT_DEADLINE_MISSING"}
		}
	} else {
		deadline, err := parseUtc(*me.GrantResolution.EffectiveDeadline)
		if err != nil || !deadline.After(now) {
			return AdmissionDecision{StopDataPlane: true, Reason: "RIGHT_EXPIRED"}
		}
	}
	decision := GrantFromGateway(catalog, gatewayID)
	if decision.Admitted && !decision.Grant.NotAfter.After(now) {
		return AdmissionDecision{StopDataPlane: true, Reason: "GRANT_EXPIRED", Grant: decision.Grant}
	}
	return decision
}

// NextRefreshAt is the single-owner refresh cadence: the verified catalog TTL, never
// earlier than the floor guard. A revoked/expired right is not extended by a lost
// refresh; last-good metadata only preserves display and selection.
func NextRefreshAt(catalog CatalogResponse, now time.Time, floor time.Duration) time.Time {
	validUntil, err := parseUtc(catalog.ValidUntil)
	if err != nil {
		return now.Add(floor)
	}
	next := validUntil
	if floor > 0 && next.Before(now.Add(floor)) {
		next = now.Add(floor)
	}
	return next
}

// BrowseRefreshAt is the display catalog refresh cadence: the browse validity, never
// earlier than the floor guard. The display list is not an admission grant and its
// refresh never extends a right.
func BrowseRefreshAt(browse BrowseCatalogResponse, now time.Time, floor time.Duration) time.Time {
	validUntil, err := parseUtc(browse.ValidUntil)
	if err != nil {
		return now.Add(floor)
	}
	next := validUntil
	if floor > 0 && next.Before(now.Add(floor)) {
		next = now.Add(floor)
	}
	return next
}

// RetryDelay mirrors the existing native profile (3 bounded attempts, jittered
// exponential) and honors the real server retry_after_ms with the parser cap of 1 hour.
func RetryDelay(attempt int, retryAfterMS *int, jitter func(time.Duration) time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	maximum := time.Second * time.Duration(1<<attempt)
	delay := time.Duration(0)
	if jitter != nil {
		delay = jitter(maximum)
	} else {
		delay = maximum / 2
	}
	if retryAfterMS != nil {
		server := time.Duration(*retryAfterMS) * time.Millisecond
		if server > time.Hour {
			server = time.Hour
		}
		if server > delay {
			delay = server
		}
	}
	if delay < 0 {
		return 0
	}
	return delay
}

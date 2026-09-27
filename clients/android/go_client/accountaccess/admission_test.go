package accountaccess

import (
	"strings"
	"testing"
	"time"
)

func admissionCatalog(t *testing.T) CatalogResponse {
	t.Helper()
	catalog, err := DecodeCatalogStrict([]byte(catalogBody("4", 1)))
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func admissionMe(t *testing.T, dataAccess string, deadline string, accountState string) MeResponse {
	t.Helper()
	binding := "1"
	body := meBody("7", &binding, "active")
	if dataAccess != "onboarding_hour" {
		body = strings.Replace(body, `"data_access":"onboarding_hour"`, `"data_access":"`+dataAccess+`"`, 1)
	}
	if dataAccess == "none" || dataAccess == "restricted_checkout" {
		body = strings.Replace(body, `"effective_deadline":"2026-09-21T11:00:00Z"`, `"effective_deadline":null`, 1)
	} else if deadline != "2026-09-21T11:00:00Z" {
		body = strings.Replace(body, `"effective_deadline":"2026-09-21T11:00:00Z"`, `"effective_deadline":"`+deadline+`"`, 1)
	}
	if accountState != "ACTIVE_TRIAL" {
		body = strings.Replace(body, `"account_state":"ACTIVE_TRIAL"`, `"account_state":"`+accountState+`"`, 1)
	}
	me, err := DecodeMeStrict([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return me
}

func TestAdmissionRequiresVerifiedRightAndLeaseSeq(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	catalog := admissionCatalog(t)
	me := admissionMe(t, "subscription_data", "2026-09-28T09:00:00Z", "ACTIVE_TRIAL")

	decision := DecideAdmission(me, catalog, "gw-synth-0", now)
	if !decision.Admitted || decision.Grant == nil || decision.Grant.LeaseSeq != "1" {
		t.Fatalf("admitted decision wrong: %+v", decision)
	}
	if decision.Grant.NodeID != "gw-synth-0" || decision.Grant.RegistrationID != "dev-synth-0" {
		t.Fatalf("grant mapping wrong: %+v", decision.Grant)
	}

	// A manually built response without lease_seq (strict decode already rejects one)
	// stays an explicit malformed-contract dependency, never a silent admission.
	malformed := admissionCatalog(t)
	malformed.Gateways[0].Access.LeaseSeq = ""
	withoutLease := DecideAdmission(me, malformed, "gw-synth-0", now)
	if withoutLease.Admitted || withoutLease.Dependency == "" || withoutLease.Reason != "LEASE_SEQ_MISSING" {
		t.Fatalf("missing lease_seq must be an explicit dependency, not a silent admission: %+v", withoutLease)
	}
}

func admissionFacts(t *testing.T, accountState, binding, entitlementType, entitlementStatus, dataAccess, deadline string) MeResponse {
	t.Helper()
	bindingRevision := "1"
	body := meBody("7", &bindingRevision, "active")
	body = strings.Replace(body, `"account_state":"ACTIVE_TRIAL"`, `"account_state":"`+accountState+`"`, 1)
	body = strings.Replace(body, `"binding_status":"active"`, `"binding_status":"`+binding+`"`, 1)
	body = strings.Replace(body, `"type":"trial"`, `"type":"`+entitlementType+`"`, 1)
	body = strings.Replace(body, `"status":"active"`, `"status":"`+entitlementStatus+`"`, 1)
	if dataAccess != "onboarding_hour" {
		body = strings.Replace(body, `"data_access":"onboarding_hour"`, `"data_access":"`+dataAccess+`"`, 1)
	}
	if dataAccess == "none" || dataAccess == "restricted_checkout" {
		body = strings.Replace(body, `"effective_deadline":"2026-09-21T11:00:00Z"`, `"effective_deadline":null`, 1)
	} else if deadline != "2026-09-21T11:00:00Z" {
		body = strings.Replace(body, `"effective_deadline":"2026-09-21T11:00:00Z"`, `"effective_deadline":"`+deadline+`"`, 1)
	}
	me, err := DecodeMeStrict([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return me
}

func admissionHourMe(t *testing.T, accountState, onboardingState string, deadline *string) MeResponse {
	t.Helper()
	bindingRevision := "1"
	body := meBody("7", &bindingRevision, "active")
	body = strings.Replace(body, `"account_state":"ACTIVE_TRIAL"`, `"account_state":"`+accountState+`"`, 1)
	body = strings.Replace(body, `"data_access":"onboarding_hour"`, `"data_access":"onboarding_hour"`, 1)
	body = strings.Replace(body, `"type":"trial"`, `"type":"none"`, 1)
	body = strings.Replace(body, `"status":"active"`, `"status":"none"`, 1)
	if deadline != nil {
		body = strings.Replace(body, `"effective_deadline":"2026-09-21T11:00:00Z"`, `"effective_deadline":"`+*deadline+`"`, 1)
	}
	me, err := DecodeMeStrict([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if deadline == nil {
		// The strict decoder rejects a null onboarding deadline; build the malformed
		// snapshot explicitly to prove admission stays fail-closed on it.
		me.GrantResolution.EffectiveDeadline = nil
	}
	me.Onboarding.State = onboardingState
	return me
}

func TestAdmissionExpiredAccountKeepsConfirmedOnboardingHour(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	catalog := admissionCatalog(t)

	future := "2026-09-21T13:00:00Z"
	decision := DecideAdmission(admissionHourMe(t, "EXPIRED", "active", &future), catalog, "gw-synth-0", now)
	if !decision.Admitted || decision.StopDataPlane {
		t.Fatalf("confirmed active hour must outlive an expired commercial subscription: %+v", decision)
	}

	past := "2026-09-21T11:00:00Z"
	if decision := DecideAdmission(admissionHourMe(t, "EXPIRED", "active", &past), catalog, "gw-synth-0", now); !decision.StopDataPlane || decision.Reason != "ACCOUNT_EXPIRED" {
		t.Fatalf("expired hour must not bypass account expiry: %+v", decision)
	}
	if decision := DecideAdmission(admissionHourMe(t, "EXPIRED", "active", nil), catalog, "gw-synth-0", now); !decision.StopDataPlane || decision.Reason != "ACCOUNT_EXPIRED" {
		t.Fatalf("malformed hour deadline must not bypass account expiry: %+v", decision)
	}
	if decision := DecideAdmission(admissionHourMe(t, "EXPIRED", "expired", &future), catalog, "gw-synth-0", now); !decision.StopDataPlane || decision.Reason != "ACCOUNT_EXPIRED" {
		t.Fatalf("inactive onboarding hour must not bypass account expiry: %+v", decision)
	}
	if decision := DecideAdmission(admissionHourMe(t, "EXPIRED", "not_started", &future), catalog, "gw-synth-0", now); !decision.StopDataPlane || decision.Reason != "ACCOUNT_EXPIRED" {
		t.Fatalf("not_started onboarding must not bypass account expiry: %+v", decision)
	}
	if decision := DecideAdmission(admissionHourMe(t, "REVOKED_SESSION", "active", &future), catalog, "gw-synth-0", now); !decision.StopDataPlane || decision.Reason != "SESSION_REVOKED" {
		t.Fatalf("revoked session must never be bypassed by an hour: %+v", decision)
	}
	revokedBinding := admissionHourMe(t, "EXPIRED", "active", &future)
	revokedBinding.BindingStatus = "revoked"
	if decision := DecideAdmission(revokedBinding, catalog, "gw-synth-0", now); !decision.StopDataPlane || decision.Reason != "BINDING_REVOKED" {
		t.Fatalf("revoked binding must never be bypassed by an hour: %+v", decision)
	}
	revokedEntitlement := admissionHourMe(t, "EXPIRED", "active", &future)
	revokedEntitlement.Entitlement.Status = "revoked"
	if decision := DecideAdmission(revokedEntitlement, catalog, "gw-synth-0", now); !decision.StopDataPlane || decision.Reason != "ENTITLEMENT_REVOKED" {
		t.Fatalf("revoked entitlement must never be bypassed by an hour: %+v", decision)
	}
	if decision := DecideAdmission(admissionMe(t, "subscription_data", "2030-01-01T00:00:00Z", "EXPIRED"), catalog, "gw-synth-0", now); !decision.StopDataPlane || decision.Reason != "ACCOUNT_EXPIRED" {
		t.Fatalf("expired subscription_data without hour stays stopped: %+v", decision)
	}
}

func TestAdmissionCatalogValidityFence(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	me := admissionMe(t, "subscription_data", "2030-01-01T00:00:00Z", "ACTIVE_PAID")

	expired := admissionCatalog(t)
	expired.ValidUntil = "2020-01-01T00:00:00Z"
	decision := DecideAdmission(me, expired, "gw-synth-0", now)
	if decision.Admitted || !decision.StopDataPlane || decision.Reason != "CATALOG_EXPIRED" {
		t.Fatalf("stale catalog must stop the data plane: %+v", decision)
	}

	invalid := admissionCatalog(t)
	invalid.ValidUntil = "not-a-time"
	if decision := DecideAdmission(me, invalid, "gw-synth-0", now); decision.Admitted || decision.Reason != "CATALOG_INVALID" {
		t.Fatalf("invalid catalog timestamp must never be admitted: %+v", decision)
	}

	fresh := admissionCatalog(t)
	if decision := DecideAdmission(me, fresh, "gw-synth-0", now); !decision.Admitted {
		t.Fatalf("fresh catalog must admit: %+v", decision)
	}
}

func TestAdmissionRevokedBindingAndEntitlementStop(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	catalog := admissionCatalog(t)
	cases := []struct {
		name   string
		me     MeResponse
		reason string
	}{
		{"binding revoked", admissionFacts(t, "ACTIVE_PAID", "revoked", "paid", "active", "subscription_data", "2030-01-01T00:00:00Z"), "BINDING_REVOKED"},
		{"binding deactivated", admissionFacts(t, "ACTIVE_PAID", "deactivated", "paid", "active", "subscription_data", "2030-01-01T00:00:00Z"), "BINDING_DEACTIVATED"},
		{"entitlement revoked", admissionFacts(t, "ACTIVE_PAID", "active", "paid", "revoked", "subscription_data", "2030-01-01T00:00:00Z"), "ENTITLEMENT_REVOKED"},
		{"entitlement review", admissionFacts(t, "ACTIVE_PAID", "active", "paid", "unknown_review", "subscription_data", "2030-01-01T00:00:00Z"), "ENTITLEMENT_UNREVIEWED"},
		{"subscription without active binding", admissionFacts(t, "VERIFIED_NO_SLOT", "none", "none", "none", "subscription_data", "2030-01-01T00:00:00Z"), "SUBSCRIPTION_NOT_ACTIVE"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decision := DecideAdmission(testCase.me, catalog, "gw-synth-0", now)
			if decision.Admitted || !decision.StopDataPlane || decision.Reason != testCase.reason {
				t.Fatalf("expected stop %s, got %+v", testCase.reason, decision)
			}
		})
	}

	// Legitimate onboarding: no paid subscription and no binding yet, but a valid
	// one-time onboarding_hour right and a verified catalog.
	onboarding := admissionFacts(t, "VERIFIED_NO_ENTITLEMENT", "none", "none", "none", "onboarding_hour", "2026-09-21T13:00:00Z")
	decision := DecideAdmission(onboarding, catalog, "gw-synth-0", now)
	if !decision.Admitted {
		t.Fatalf("onboarding_hour must not require a paid subscription: %+v", decision)
	}
}

func TestAdmissionStopsDataPlaneOnRevokeExpiryAndNoRight(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	catalog := admissionCatalog(t)
	cases := []struct {
		name         string
		me           MeResponse
		gateway      string
		reasonWanted string
	}{
		{"no right", admissionMe(t, "none", "2026-09-21T11:00:00Z", "ACTIVE_TRIAL"), "gw-synth-0", "DATA_ACCESS_NONE"},
		{"checkout only", admissionMe(t, "restricted_checkout", "2026-09-21T11:00:00Z", "ACTIVE_TRIAL"), "gw-synth-0", "RESTRICTED_CHECKOUT"},
		{"account expired", admissionMe(t, "subscription_data", "2026-09-28T09:00:00Z", "EXPIRED"), "gw-synth-0", "ACCOUNT_EXPIRED"},
		{"session revoked", admissionMe(t, "subscription_data", "2026-09-28T09:00:00Z", "REVOKED_SESSION"), "gw-synth-0", "SESSION_REVOKED"},
		{"right expired", admissionMe(t, "subscription_data", "2026-09-21T11:00:00Z", "ACTIVE_TRIAL"), "gw-synth-0", "RIGHT_EXPIRED"},
		{"node removed", admissionMe(t, "subscription_data", "2026-09-28T09:00:00Z", "ACTIVE_TRIAL"), "gw-missing", "SELECTED_NODE_REMOVED"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decision := DecideAdmission(testCase.me, catalog, testCase.gateway, now)
			if decision.Admitted || !decision.StopDataPlane || decision.Reason != testCase.reasonWanted {
				t.Fatalf("expected stop %s, got %+v", testCase.reasonWanted, decision)
			}
		})
	}
}

func TestRetryDelayIsBoundedAtOneHour(t *testing.T) {
	zero := func(time.Duration) time.Duration { return 0 }
	if delay := RetryDelay(1, nil, zero); delay != 0 {
		t.Fatalf("jitter-free attempt delay=%v", delay)
	}
	ninety := 90 * 60 * 1000
	if delay := RetryDelay(1, &ninety, zero); delay != time.Hour {
		t.Fatalf("retry_after must be capped at the parser cap 1h, got %v", delay)
	}
	five := 5000
	if delay := RetryDelay(1, &five, zero); delay != 5*time.Second {
		t.Fatalf("server retry_after must be honored, got %v", delay)
	}
}

func TestNextRefreshAtUsesCatalogTTLWithFloor(t *testing.T) {
	catalog := admissionCatalog(t)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	next := NextRefreshAt(catalog, now, time.Minute)
	if !next.Equal(time.Date(2026, 9, 21, 12, 10, 0, 0, time.UTC)) {
		t.Fatalf("catalog valid_until must drive refresh, got %v", next)
	}
	expiredTTL := catalog
	expiredTTL.ValidUntil = "2026-09-21T11:00:00Z"
	if next := NextRefreshAt(expiredTTL, now, time.Minute); !next.Equal(now.Add(time.Minute)) {
		t.Fatalf("floor guard must apply for a stale TTL, got %v", next)
	}
}

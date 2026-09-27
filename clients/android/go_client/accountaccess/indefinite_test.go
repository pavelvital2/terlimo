package accountaccess

// Indefinite subscription_data contract: a perpetual commercial entitlement without
// valid_until may carry a null effective_deadline. Everything else stays finite and
// fail-closed, and the verified catalog validity / node lease remain the stop bounds.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// indefiniteMeFixture reproduces the exact real backend /me structure observed for the
// installed client: schema_version 1.0, ACTIVE_PAID, active rev1 binding, indefinite
// paid entitlement (perpetual_commercial true, valid_until null), subscription_data
// with a null effective_deadline and not_started onboarding.
const indefiniteMeFixture = `{
	"request_id":"0123456789abcdef0123456789abcdef",
	"server_time":"2026-09-22T15:35:00Z",
	"schema_version":"1.0",
	"status":"ok",
	"account_state":"ACTIVE_PAID",
	"telegram_linked":true,
	"entitlement":{"type":"paid","status":"active","valid_from":"2026-09-22T15:33:15Z",
		"valid_until":null,"effective_device_limit":2,"slots_used":1,
		"revision":"1","perpetual_commercial":true},
	"binding_status":"active",
	"binding_revision":"1",
	"management_only":false,
	"account_ref":null,
	"onboarding":{"state":"not_started","started_by":"server_confirmed_first_connection",
		"started_at":null,"not_after":null,"duration_seconds":3600,"one_time":true,
		"extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
		"requires_hardware_id":false,"unit":"installation_fingerprint",
		"post_telegram_identity":"account_history_correlation",
		"pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
	"grant_resolution":{"control_available":true,"restricted_checkout_available":true,
		"data_access":"subscription_data","effective_deadline":null},
	"revision":"7"
}`

func TestDecodeMeStrictAcceptsIndefiniteSubscriptionData(t *testing.T) {
	me, err := DecodeMeStrict([]byte(indefiniteMeFixture))
	if err != nil {
		t.Fatalf("real indefinite /me rejected by strict decode: %v", err)
	}
	if me.GrantResolution.DataAccess != "subscription_data" || me.GrantResolution.EffectiveDeadline != nil ||
		!me.Entitlement.PerpetualCommercial || me.Entitlement.ValidUntil != nil {
		t.Fatalf("indefinite fixture decoded into unexpected shape: %+v", me)
	}
	if !IndefiniteSubscriptionData(me) {
		t.Fatal("accepted indefinite shape must report the contract rule")
	}
}

func TestIndefiniteSubscriptionDataRule(t *testing.T) {
	finite := "2027-09-22T15:33:15Z"
	me, err := DecodeMeStrict([]byte(indefiniteMeFixture))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*MeResponse)
		want   bool
	}{
		{"perpetual without valid_until", func(*MeResponse) {}, true},
		{"perpetual false", func(m *MeResponse) { m.Entitlement.PerpetualCommercial = false }, false},
		{"valid_until present", func(m *MeResponse) { m.Entitlement.ValidUntil = &finite }, false},
		{"data access none", func(m *MeResponse) { m.GrantResolution.DataAccess = "none" }, false},
		{"onboarding hour", func(m *MeResponse) { m.GrantResolution.DataAccess = "onboarding_hour" }, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			copied := me
			testCase.mutate(&copied)
			if got := IndefiniteSubscriptionData(copied); got != testCase.want {
				t.Fatalf("IndefiniteSubscriptionData=%v want %v", got, testCase.want)
			}
		})
	}
}

func TestDecodeMeStrictRejectsNullDeadlineOutsideTheIndefiniteRule(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
	}{
		{"perpetual false", func(body string) string {
			return strings.Replace(body, `"perpetual_commercial":true`, `"perpetual_commercial":false`, 1)
		}},
		{"valid_until present", func(body string) string {
			return strings.Replace(body, `"valid_until":null`, `"valid_until":"2027-09-22T15:33:15Z"`, 1)
		}},
		{"onboarding hour with null deadline", func(body string) string {
			return strings.Replace(body, `"data_access":"subscription_data"`, `"data_access":"onboarding_hour"`, 1)
		}},
		{"malformed deadline", func(body string) string {
			return strings.Replace(body, `"effective_deadline":null`, `"effective_deadline":"later"`, 1)
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := testCase.mutate(indefiniteMeFixture)
			if body == indefiniteMeFixture {
				t.Fatal("fixture mutation did not apply")
			}
			if _, err := DecodeMeStrict([]byte(body)); err == nil {
				t.Fatal("null deadline outside the indefinite rule must stay malformed")
			}
		})
	}
	// The finite subscription path keeps decoding, including a perpetual entitlement
	// that still carries an explicit finite deadline.
	finite := strings.Replace(indefiniteMeFixture, `"effective_deadline":null`, `"effective_deadline":"2027-09-22T15:33:15Z"`, 1)
	if _, err := DecodeMeStrict([]byte(finite)); err != nil {
		t.Fatalf("finite subscription_data rejected: %v", err)
	}
}

func TestAdmissionIndefiniteSubscriptionDataFailsClosedAndAdmits(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	catalog := admissionCatalog(t)
	me, err := DecodeMeStrict([]byte(indefiniteMeFixture))
	if err != nil {
		t.Fatal(err)
	}

	decision := DecideAdmission(me, catalog, "gw-synth-0", now)
	if !decision.Admitted || decision.Grant == nil || decision.Grant.LeaseSeq != "1" {
		t.Fatalf("indefinite subscription_data must admit with a fresh catalog: %+v", decision)
	}

	cases := []struct {
		name   string
		mutate func(*MeResponse, *CatalogResponse)
		reason string
	}{
		{"inconsistent null: perpetual false", func(m *MeResponse, _ *CatalogResponse) {
			m.Entitlement.PerpetualCommercial = false
		}, "RIGHT_DEADLINE_MISSING"},
		{"inconsistent null: valid_until present", func(m *MeResponse, _ *CatalogResponse) {
			value := "2027-09-22T15:33:15Z"
			m.Entitlement.ValidUntil = &value
		}, "RIGHT_DEADLINE_MISSING"},
		{"onboarding with null deadline", func(m *MeResponse, _ *CatalogResponse) {
			m.GrantResolution.DataAccess = "onboarding_hour"
		}, "RIGHT_DEADLINE_MISSING"},
		{"entitlement revoked", func(m *MeResponse, _ *CatalogResponse) {
			m.Entitlement.Status = "revoked"
		}, "ENTITLEMENT_REVOKED"},
		{"expired catalog", func(_ *MeResponse, c *CatalogResponse) {
			c.ValidUntil = "2026-09-21T11:00:00Z"
		}, "CATALOG_EXPIRED"},
		{"expired node lease", func(_ *MeResponse, c *CatalogResponse) {
			c.Gateways[0].Access.NotAfter = "2026-09-21T11:00:00Z"
		}, "GRANT_EXPIRED"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			copiedMe, copiedCatalog := me, catalog
			testCase.mutate(&copiedMe, &copiedCatalog)
			decision := DecideAdmission(copiedMe, copiedCatalog, "gw-synth-0", now)
			if decision.Admitted || !decision.StopDataPlane || decision.Reason != testCase.reason {
				t.Fatalf("expected stop %s, got %+v", testCase.reason, decision)
			}
		})
	}
}

func TestCoordinatorIndefiniteMeReachesGatewaysAndSync(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, store := newTestCoordinator(t, fake, &subject)
	fake.meBody = indefiniteMeFixture
	fake.gatewayBody = catalogBody("4", 1)
	fake.syncResponse = syncBody("op-indef-1", "applied")

	result, err := coordinator.Refresh(context.Background())
	if err != nil || result.State != StateApplied || result.Projection == nil {
		t.Fatalf("indefinite /me must refresh and project: %v %+v", err, result)
	}
	if fake.gatewayCalls != 0 {
		t.Fatalf("refresh must not touch /gateways: %d", fake.gatewayCalls)
	}
	grant, _ := result.Projection.Payload()["grant_resolution"].(map[string]any)
	if grant["effective_deadline"] != nil || grant["data_access"] != "subscription_data" {
		t.Fatalf("projection must carry the indefinite grant verbatim: %+v", grant)
	}

	catalogResult, err := coordinator.RefreshCatalog(context.Background())
	if err != nil || !catalogResult.Applied || catalogResult.Catalog == nil || len(catalogResult.Catalog.Gateways) != 1 {
		t.Fatalf("indefinite /me must reach the verified catalog: %v %+v", err, catalogResult)
	}
	if fake.gatewayCalls != 1 {
		t.Fatalf("GET /gateways calls=%d want 1", fake.gatewayCalls)
	}

	sync, err := coordinator.SyncAccess(context.Background())
	if err != nil || sync.Receipt == nil || sync.Receipt.Response == nil ||
		sync.Receipt.Response.AccessApplicationState != "applied" {
		t.Fatalf("indefinite right must not block the sync path: %v %+v", err, sync)
	}
	if fake.syncCalls != 1 || store.Value == nil {
		t.Fatalf("sync must be effect-once: calls=%d stored=%v", fake.syncCalls, store.Value != nil)
	}
}

package accountaccess

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCompareRevisionIsNumericAndArbitraryPrecision(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2", "10", -1},
		{"10", "2", 1},
		{"7", "7", 0},
		{"0", "0", 0},
		{"9999999999999999998", "9999999999999999999", -1},
		{"9999999999999999999", "9999999999999999998", 1},
	}
	for _, testCase := range cases {
		got, ok := CompareRevision(testCase.a, testCase.b)
		if !ok || got != testCase.want {
			t.Fatalf("CompareRevision(%q,%q)=%d,%v want %d,true", testCase.a, testCase.b, got, ok, testCase.want)
		}
	}
	for _, invalid := range []string{"", "01", "-1", "1.0", "1e3", "١٢٣"} {
		if _, ok := CompareRevision(invalid, "1"); ok {
			t.Fatalf("invalid revision %q accepted", invalid)
		}
	}
}

func TestUtcTimeRejectsOffsetsAndCoarseValues(t *testing.T) {
	if !ValidUtcTime("2026-09-21T12:00:00Z") || !ValidUtcTime("2026-09-21T12:00:00.123456789Z") {
		t.Fatal("valid UtcTime rejected")
	}
	for _, invalid := range []string{"2026-09-21T12:00:00+00:00", "2026-09-21T12:00Z", "2026-09-21 12:00:00Z"} {
		if ValidUtcTime(invalid) {
			t.Fatalf("invalid UtcTime %q accepted", invalid)
		}
	}
}

func meBody(revision string, bindingRevision *string, onboardingState string) string {
	binding := "null"
	if bindingRevision != nil {
		binding = fmt.Sprintf("%q", *bindingRevision)
	}
	startedAt, notAfter := "null", "null"
	if onboardingState != "not_started" {
		startedAt = `"2026-09-21T10:00:00Z"`
		notAfter = `"2026-09-21T11:00:00Z"`
	}
	dataAccess, deadline := "none", "null"
	if onboardingState == "active" {
		dataAccess = "onboarding_hour"
		deadline = `"2026-09-21T11:00:00Z"`
	}
	return fmt.Sprintf(`{
		"request_id":"0123456789abcdef0123456789abcdef",
		"server_time":"2026-09-21T12:00:00Z",
		"schema_version":"1.0",
		"status":"ok",
		"account_state":"ACTIVE_TRIAL",
		"telegram_linked":true,
		"entitlement":{"type":"trial","status":"active","valid_from":"2026-09-21T09:00:00Z",
			"valid_until":"2026-09-28T09:00:00Z","effective_device_limit":2,"slots_used":1,
			"revision":"3","perpetual_commercial":false},
		"binding_status":"active",
		"binding_revision":%s,
		"management_only":false,
		"onboarding":{"state":%q,"started_by":"server_confirmed_first_connection",
			"started_at":%s,"not_after":%s,"duration_seconds":3600,"one_time":true,
			"extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
			"requires_hardware_id":false,"unit":"installation_fingerprint",
			"post_telegram_identity":"account_history_correlation",
			"pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
		"grant_resolution":{"control_available":true,"restricted_checkout_available":true,
			"data_access":%q,"effective_deadline":%s},
		"revision":%q
	}`, binding, onboardingState, startedAt, notAfter, dataAccess, deadline, revision)
}

func TestDecodeMeStrictAcceptsContractAndRejectsAdditions(t *testing.T) {
	binding := "1"
	me, err := DecodeMeStrict([]byte(meBody("7", &binding, "active")))
	if err != nil {
		t.Fatalf("valid me rejected: %v", err)
	}
	if me.Revision != "7" || me.BindingRevision == nil || *me.BindingRevision != "1" {
		t.Fatalf("unexpected me projection: %+v", me)
	}
	if _, err := DecodeMeStrict([]byte(strings.Replace(meBody("7", &binding, "active"), `"revision":"7"`, `"revision":"7","extra":1`, 1))); err == nil {
		t.Fatal("additive field accepted")
	}
}

func TestBindingRevisionMustBeNonZeroOrNull(t *testing.T) {
	zero := "0"
	if _, err := DecodeMeStrict([]byte(meBody("7", &zero, "active"))); err == nil {
		t.Fatal("binding_revision=0 accepted")
	}
	if _, err := DecodeMeStrict([]byte(meBody("7", &zero, "active"))); err == nil {
		t.Fatal("binding_revision equal to subject revision accepted")
	}
	if _, err := DecodeMeStrict([]byte(meBody("7", nil, "not_started"))); err != nil {
		t.Fatalf("null binding must decode: %v", err)
	}
}

func catalogBody(revision string, gatewayCount int) string {
	nodes := make([]string, 0, gatewayCount)
	for index := 0; index < gatewayCount; index++ {
		nodes = append(nodes, fmt.Sprintf(`{
			"gateway_id":"gw-synth-%d","name":"Synthetic %d","region":"test","country_code":"XX",
			"capabilities":["managed"],"target_workers":36,
			"transport":{"protocol":"wdtt-v17","peer_ip":"127.0.0.1","dtls_port":56300,"wg_port":56302,
				"dtls_spki_sha256":"PAyFy4YlbMsIfA4GfHvX_r8-HUWKVuj9PUeJ4pEtAjo"},
			"access":{"grant_id":"grant-synth-%d","device_ref":"dev-synth-%d","password":"synthetic",
				"generation":"1","lease_seq":"1","not_after":"2026-09-21T12:10:00Z"}}`, index, index, index, index))
	}
	return fmt.Sprintf(`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z",
		"schema_version":"1.0","status":"ok","revision":%q,"valid_until":"2026-09-21T12:10:00Z",
		"issued_at":"2026-09-21T11:55:00Z","gateways":[%s]}`, revision, strings.Join(nodes, ","))
}

func TestDecodeCatalogStrictRejectsMalformedRegisteredNode(t *testing.T) {
	catalog, err := DecodeCatalogStrict([]byte(catalogBody("4", 2)))
	if err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}
	if catalog.Gateways[0].Access.LeaseSeq != "1" {
		t.Fatalf("required lease_seq not decoded: %+v", catalog.Gateways[0].Access)
	}
	malformed := strings.Replace(catalogBody("4", 1), `"dtls_spki_sha256":"PAyFy4YlbMsIfA4GfHvX_r8-HUWKVuj9PUeJ4pEtAjo"`, `"dtls_spki_sha256":"not-base64"`, 1)
	if _, err := DecodeCatalogStrict([]byte(malformed)); err == nil {
		t.Fatal("malformed registered node accepted")
	}
	duplicated := strings.Replace(catalogBody("4", 2), `"gw-synth-1"`, `"gw-synth-0"`, 1)
	if _, err := DecodeCatalogStrict([]byte(duplicated)); err == nil {
		t.Fatal("duplicate gateway id accepted")
	}
}

func TestDecodeCatalogStrictRequiresLeaseSeq(t *testing.T) {
	without := strings.Replace(catalogBody("4", 1), `,"lease_seq":"1"`, "", 1)
	if !strings.Contains(catalogBody("4", 1), `"lease_seq":"1"`) || strings.Contains(without, "lease_seq") {
		t.Fatal("fixture rewrite failed")
	}
	if _, err := DecodeCatalogStrict([]byte(without)); err == nil {
		t.Fatal("catalog without lease_seq must be malformed")
	}
	zero := strings.Replace(catalogBody("4", 1), `"lease_seq":"1"`, `"lease_seq":"0"`, 1)
	if _, err := DecodeCatalogStrict([]byte(zero)); err == nil {
		t.Fatal("lease_seq=0 must be malformed")
	}
	null := strings.Replace(catalogBody("4", 1), `"lease_seq":"1"`, `"lease_seq":null`, 1)
	if _, err := DecodeCatalogStrict([]byte(null)); err == nil {
		t.Fatal("null lease_seq must be malformed")
	}
}

func TestErrorEnvelopeAdmissionTokens(t *testing.T) {
	body := `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z",
		"schema_version":"1.0","status":"error","code":"ACCESS_SYNC_PENDING","retryable":true,
		"retry_after_ms":1500,"details":{"catalog_revision":"4","binding_revision":"1"}}`
	envelope, err := DecodeErrorStrict([]byte(body))
	if err != nil {
		t.Fatalf("valid error rejected: %v", err)
	}
	catalogRevision, bindingRevision, ok := envelope.AdmissionTokens()
	if !ok || catalogRevision != "4" || bindingRevision != "1" {
		t.Fatalf("admission tokens=%q,%q,%v", catalogRevision, bindingRevision, ok)
	}
}

func TestProjectionDigestIsCanonicalAndNFCSensitive(t *testing.T) {
	first := Projection{
		ServerTime: "2026-09-21T12:00:00Z", AccessRevision: "7",
		Account:         map[string]any{"state": "ACTIVE_TRIAL", "telegram_linked": true, "binding_status": "active", "management_only": false, "account_ref": nil},
		Entitlement:     map[string]any{"type": "trial", "status": "active", "revision": "3"},
		Onboarding:      map[string]any{"state": "active"},
		GrantResolution: map[string]any{"data_access": "subscription_data"},
	}
	second := Projection{
		ServerTime: "2026-09-22T00:00:00Z", AccessRevision: "7",
		Account:         map[string]any{"account_ref": nil, "management_only": false, "binding_status": "active", "telegram_linked": true, "state": "ACTIVE_TRIAL"},
		Entitlement:     map[string]any{"revision": "3", "status": "active", "type": "trial"},
		Onboarding:      map[string]any{"state": "active"},
		GrantResolution: map[string]any{"data_access": "subscription_data"},
	}
	firstDigest, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := second.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("canonical digest must ignore key order and non-protected server_time: %s vs %s", firstDigest, secondDigest)
	}
	first.Account["state"] = "ACTIVE_PAID"
	changed, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if changed == firstDigest {
		t.Fatal("protected payload change must change the digest")
	}
}

func TestProjectionPayloadHasExactFrozenKeySet(t *testing.T) {
	projection := Projection{ServerTime: "2026-09-21T12:00:00Z", AccessRevision: "7",
		Account:     map[string]any{"state": "ACTIVE_TRIAL", "telegram_linked": true, "binding_status": "active", "management_only": false, "account_ref": nil},
		Entitlement: map[string]any{"type": "trial"}, Onboarding: map[string]any{"state": "active"},
		GrantResolution: map[string]any{"data_access": "subscription_data"}}
	raw, err := json.Marshal(projection.Envelope("attempt"))
	if err != nil {
		t.Fatal(err)
	}
	decoded := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	// S3-A registration and S3-B trial are the accepted additive display blocks.
	want := []string{"v", "attempt_id", "type", "access_version", "server_time", "session_generation",
		"previous_session_generation", "access_revision", "account", "entitlement", "onboarding", "grant_resolution",
		"registration", "trial"}
	if len(decoded) != len(want) {
		t.Fatalf("frozen key count=%d want %d: %s", len(decoded), len(want), raw)
	}
	for _, key := range want {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("missing frozen key %q", key)
		}
	}
	if string(decoded["type"]) != `"account_access"` || string(decoded["access_version"]) != "1" {
		t.Fatalf("frozen constants changed: %s", raw)
	}
}

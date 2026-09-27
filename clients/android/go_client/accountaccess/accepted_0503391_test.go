package accountaccess

// Contract pack 0503391 (relay delivery step035-contract-0503391.tar.gz, archive sha256
// d2c05876c38eb69033e2c2386ef97ead981a54b8b9d798a9efe586f42e3d3e92) carries the schema/test
// authority for the perpetual-deadline rule D2 implements: a NULL
// grant_resolution.effective_deadline is valid only for data_access=subscription_data with
// entitlement.perpetual_commercial=true and entitlement.valid_until=null.
//
// The archive has no standalone /me fixture or projection vector (only schemas/, mapping/ and
// tests/), so the exact schema/test bytes were copied into testdata/accepted-0503391 and
// pinned. The decode acceptance below anchors on the observed indefiniteMeFixture from the D2
// tests and cross-checks it against the pinned 0503391 rule; no fixture bytes are invented.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const (
	accepted0503391ContractID    = "050339123bb77d1ebda866000d2498e3be5a9b77"
	accepted0503391TarballSHA256 = "d2c05876c38eb69033e2c2386ef97ead981a54b8b9d798a9efe586f42e3d3e92"
)

func accepted0503391Files() []string {
	return []string{
		accepted0503391Dir + "mapping/COMPATIBILITY_NOTES.md",
		accepted0503391Dir + "schemas/common.json",
		accepted0503391Dir + "schemas/onboarding.json",
		accepted0503391Dir + "schemas/subscription.json",
		accepted0503391Dir + "tests/test_perpetual_deadline.py",
	}
}

func pinnedJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(readAcceptedFixture(t, path), &doc); err != nil {
		t.Fatalf("%s is not a JSON object: %v", path, err)
	}
	return doc
}

func jsonPath(t *testing.T, node any, keys ...string) any {
	t.Helper()
	for _, key := range keys {
		object, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("0503391 schema: %q parent is not an object", key)
		}
		node, ok = object[key]
		if !ok {
			t.Fatalf("0503391 schema: missing %q", key)
		}
	}
	return node
}

func TestAccepted0503391IndefiniteMeFixtureDecodes(t *testing.T) {
	me, err := DecodeMeStrict([]byte(indefiniteMeFixture))
	if err != nil {
		t.Fatalf("observed perpetual indefinite /me rejected: %v", err)
	}
	if me.GrantResolution.DataAccess != "subscription_data" || me.GrantResolution.EffectiveDeadline != nil ||
		!me.Entitlement.PerpetualCommercial || me.Entitlement.ValidUntil != nil {
		t.Fatalf("perpetual indefinite shape decoded wrong: %+v", me)
	}
	if !IndefiniteSubscriptionData(me) {
		t.Fatal("perpetual indefinite shape must report the 0503391 contract rule")
	}
}

func TestAccepted0503391NegativesFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
	}{
		{"perpetual false with null deadline", func(body string) string {
			return strings.Replace(body, `"perpetual_commercial":true`, `"perpetual_commercial":false`, 1)
		}},
		{"valid_until non-null with null deadline", func(body string) string {
			return strings.Replace(body, `"valid_until":null`, `"valid_until":"2027-09-22T15:33:15Z"`, 1)
		}},
		{"onboarding_hour null deadline", func(body string) string {
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
				t.Fatal("negative outside the 0503391 perpetual rule must stay malformed")
			}
		})
	}
	finite := strings.Replace(indefiniteMeFixture, `"effective_deadline":null`, `"effective_deadline":"2027-09-22T15:33:15Z"`, 1)
	if _, err := DecodeMeStrict([]byte(finite)); err != nil {
		t.Fatalf("finite subscription_data from the 0503391 matrix rejected: %v", err)
	}
}

func TestAccepted0503391SchemaRuleMatchesGoDecoder(t *testing.T) {
	subscription := pinnedJSONObject(t, accepted0503391Dir+"schemas/subscription.json")
	allOf, ok := jsonPath(t, subscription, "$defs", "MeResponse", "allOf").([]any)
	if !ok || len(allOf) != 1 {
		t.Fatalf("0503391 MeResponse cross-object invariant missing")
	}
	if got := jsonPath(t, allOf[0], "if", "properties", "grant_resolution", "properties", "data_access", "const"); got != "subscription_data" {
		t.Fatalf("0503391 invariant condition changed: %v", got)
	}
	if got := jsonPath(t, allOf[0], "if", "properties", "grant_resolution", "properties", "effective_deadline", "type"); got != "null" {
		t.Fatalf("0503391 invariant null deadline condition changed: %v", got)
	}
	if got := jsonPath(t, allOf[0], "then", "properties", "entitlement", "properties", "perpetual_commercial", "const"); got != true {
		t.Fatalf("0503391 invariant perpetual condition changed: %v", got)
	}
	if got := jsonPath(t, allOf[0], "then", "properties", "entitlement", "properties", "valid_until", "type"); got != "null" {
		t.Fatalf("0503391 invariant valid_until condition changed: %v", got)
	}

	onboarding := pinnedJSONObject(t, accepted0503391Dir+"schemas/onboarding.json")
	rules, ok := jsonPath(t, onboarding, "$defs", "GrantResolution", "allOf").([]any)
	if !ok || len(rules) != 3 {
		t.Fatalf("0503391 GrantResolution deadline classes missing")
	}
	deadlines := map[string]string{}
	for _, item := range rules {
		access, ok := jsonPath(t, item, "if", "properties", "data_access").(map[string]any)
		if !ok {
			t.Fatal("0503391 GrantResolution condition is not an object")
		}
		class := ""
		if value, present := access["const"]; present {
			class, ok = value.(string)
			if !ok {
				t.Fatal("0503391 data_access const is not a string")
			}
		} else {
			values, ok := access["enum"].([]any)
			if !ok {
				t.Fatal("0503391 data_access condition is neither const nor enum")
			}
			names := make([]string, 0, len(values))
			for _, value := range values {
				name, ok := value.(string)
				if !ok {
					t.Fatal("0503391 data_access enum value is not a string")
				}
				names = append(names, name)
			}
			class = strings.Join(names, "|")
		}
		deadline := jsonPath(t, item, "then", "properties", "effective_deadline", "type")
		if name, ok := deadline.(string); ok {
			deadlines[class] = name
		} else {
			values, ok := deadline.([]any)
			if !ok {
				t.Fatalf("0503391 deadline type for %s is not a type list", class)
			}
			names := make([]string, 0, len(values))
			for _, value := range values {
				name, ok := value.(string)
				if !ok {
					t.Fatalf("0503391 deadline type for %s is not a string", class)
				}
				names = append(names, name)
			}
			deadlines[class] = strings.Join(names, "|")
		}
	}
	if deadlines["onboarding_hour"] != "string" || deadlines["subscription_data"] != "string|null" || deadlines["none|restricted_checkout"] != "null" {
		t.Fatalf("0503391 GrantResolution deadline classes changed: %+v", deadlines)
	}

	python := string(readAcceptedFixture(t, accepted0503391Dir+"tests/test_perpetual_deadline.py"))
	for _, name := range []string{
		"test_positive_finite_subscription",
		"test_positive_indefinite_real_builder_fixture",
		"test_negative_null_deadline_without_perpetual",
		"test_negative_null_deadline_with_non_null_valid_until",
		"test_negative_onboarding_hour_null_deadline",
		"test_negative_finite_subscription_null_deadline",
		"test_validator_applies_nested_refs_not_silently_ignored",
	} {
		if !strings.Contains(python, "def "+name+"(") {
			t.Fatalf("0503391 deadline matrix lost %s", name)
		}
	}

	notes := string(readAcceptedFixture(t, accepted0503391Dir+"mapping/COMPATIBILITY_NOTES.md"))
	if !strings.Contains(notes, "perpetual_commercial=true") || !strings.Contains(notes, "valid_until=null") {
		t.Fatal("0503391 compatibility notes lost the cross-object invariant")
	}
}

func TestAccepted0503391ProvenanceRecorded(t *testing.T) {
	for _, path := range accepted0503391Files() {
		if _, pinned := acceptedFixtureSHA256[path]; !pinned {
			t.Fatalf("%s is not pinned by acceptedFixtureSHA256", path)
		}
		readAcceptedFixture(t, path)
	}
	raw, err := os.ReadFile(accepted0503391Dir + "PROVENANCE.md")
	if err != nil {
		t.Fatal(err)
	}
	provenance := string(raw)
	for _, want := range []string{accepted0503391ContractID, accepted0503391TarballSHA256, "no standalone `/me` fixture"} {
		if !strings.Contains(provenance, want) {
			t.Fatalf("0503391 provenance record lost %q", want)
		}
	}
	for path, digest := range acceptedFixtureSHA256 {
		if strings.HasPrefix(path, accepted0503391Dir) && !strings.Contains(provenance, digest) {
			t.Fatalf("0503391 provenance record lost the pin for %s", path)
		}
	}
}

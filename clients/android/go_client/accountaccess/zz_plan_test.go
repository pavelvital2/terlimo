package accountaccess

import (
	"strings"
	"testing"
)

const planEntitlementTail = `"type": "none", "valid_from": null, "valid_until": null}`

func meWithPlan(planJSON string) string {
	return strings.Replace(hourMeActive, planEntitlementTail,
		`"type": "none", "valid_from": null, "valid_until": null, "plan": `+planJSON+`}`, 1)
}

func TestDecodeMeStrictOptionalPlan(t *testing.T) {
	// Valid plan (title present).
	me, err := DecodeMeStrict([]byte(meWithPlan(`{"id":"terlimo-30d","title":"30 дней","duration_code":"days:30"}`)))
	if err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	if me.Entitlement.Plan == nil || me.Entitlement.Plan.ID != "terlimo-30d" ||
		me.Entitlement.Plan.Title == nil || *me.Entitlement.Plan.Title != "30 дней" ||
		me.Entitlement.Plan.DurationCode != "days:30" {
		t.Fatalf("plan decoded wrong: %+v", me.Entitlement.Plan)
	}

	// null title is allowed.
	if _, err := DecodeMeStrict([]byte(meWithPlan(`{"id":"x","title":null,"duration_code":"days:30"}`))); err != nil {
		t.Fatalf("null title rejected: %v", err)
	}
	// JSON null plan is allowed and means unknown.
	meNull, err := DecodeMeStrict([]byte(meWithPlan(`null`)))
	if err != nil || meNull.Entitlement.Plan != nil {
		t.Fatalf("null plan not accepted as unknown: %v %+v", err, meNull.Entitlement.Plan)
	}
	// Absent plan (old server) still works and stays nil.
	meAbsent, err := DecodeMeStrict([]byte(hourMeActive))
	if err != nil || meAbsent.Entitlement.Plan != nil {
		t.Fatalf("absent plan not accepted: %v %+v", err, meAbsent.Entitlement.Plan)
	}
}

func TestDecodeMeStrictPlanRejections(t *testing.T) {
	cases := []string{
		`{"id":"x","title":"t","duration_code":"days:30","extra":1}`, // unknown nested key
		`{"id":"","title":"t","duration_code":"days:30"}`,           // empty id
		`{"id":"x","title":"t"}`,                                    // missing duration_code
		`{"id":"x","duration_code":"days:30"}`,                      // missing mandatory title key
		`{"id":"x","title":"` + strings.Repeat("a", 129) + `","duration_code":"days:30"}`, // oversize title
		`{"id":"x","title":"t","duration_code":"` + strings.Repeat("d", 65) + `"}`,         // oversize duration
	}
	for _, plan := range cases {
		if _, err := DecodeMeStrict([]byte(meWithPlan(plan))); err == nil {
			t.Fatalf("invalid plan accepted: %s", plan)
		}
	}
}

func TestBuildProjectionCarriesPlanOnlyWhenPresent(t *testing.T) {
	title := "30 дней"
	withPlan := buildProjection(MeResponse{Entitlement: meEntitlement{
		Type: "paid", Status: "active", Revision: "1",
		Plan: &meEntitlementPlan{ID: "terlimo-30d", Title: &title, DurationCode: "days:30"},
	}}, 1, nil)
	raw, ok := withPlan.Entitlement["plan"].(map[string]any)
	if !ok || raw["id"] != "terlimo-30d" || raw["title"] != "30 дней" || raw["duration_code"] != "days:30" {
		t.Fatalf("projection plan wrong: %+v", withPlan.Entitlement["plan"])
	}

	without := buildProjection(MeResponse{Entitlement: meEntitlement{Type: "paid", Status: "active", Revision: "1"}}, 1, nil)
	if _, present := without.Entitlement["plan"]; present {
		t.Fatalf("projection must omit plan when absent")
	}
}

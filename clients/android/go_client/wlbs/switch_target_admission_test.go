package wlbs

import (
	"testing"
	"time"
)

func TestSwitchTargetAdmissionBothDirections(t *testing.T) {
	now := time.Date(2026, 9, 13, 5, 0, 0, 0, time.UTC)
	for _, target := range []int{0, 1} {
		for _, expired := range []bool{false, true} {
			catalog := twoNodeCatalog()
			setCatalogTimes(&catalog, now)
			if expired {
				catalog.Nodes[target].Access.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
			}
			if err := catalog.Validate("fixture-only", "registration", now); err != nil {
				t.Fatal(err)
			}
			if err := catalog.ValidateNode("fixture-only", "registration", catalog.Nodes[1-target].NodeID, now); err != nil {
				t.Fatal("last-good active grant must remain eligible", err)
			}
			err := catalog.ValidateNode("fixture-only", "registration", catalog.Nodes[target].NodeID, now)
			if expired {
				wireCode(t, err, "BAD_CATALOG")
			} else if err != nil {
				t.Fatal("fresh target rejected", err)
			}
		}
	}
}

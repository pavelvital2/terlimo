package accountaccess

import (
	"context"
	"strings"
	"testing"
	"time"
)

// S5-074: a same-revision GET /gateways snapshot with a strictly newer server IssuedAt is
// a refreshed snapshot of the same revision and must be applied as-is. Before this fix a
// same revision with a different digest returned REVISION_CONFLICT with the OLD lastGood,
// so a re-issued (re-leased) catalog was never consumed and the old deadline re-armed.

func sameRevisionCatalogBody(revision, issuedAt, validUntil string, gatewayCount int) string {
	body := catalogBody(revision, gatewayCount)
	body = strings.Replace(body, `"valid_until":"2026-09-21T12:10:00Z"`, `"valid_until":"`+validUntil+`"`, 1)
	body = strings.Replace(body, `"issued_at":"2026-09-21T11:55:00Z"`, `"issued_at":"`+issuedAt+`"`, 1)
	return body
}

func newSameRevisionCoordinator(t *testing.T, fake *fakeServer) *Coordinator {
	t.Helper()
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func TestSameRevisionNewerIssuedAtReExtendsCatalog(t *testing.T) {
	const (
		revision = "90"
		issuedV1 = "2026-09-21T11:55:00Z"
		validV1  = "2026-09-21T12:10:00Z"
		issuedV2 = "2026-09-21T12:10:00Z"
		validV2  = "2026-09-21T12:20:00Z"
	)
	fake := &fakeServer{}
	coordinator := newSameRevisionCoordinator(t, fake)
	fake.gatewayBody = sameRevisionCatalogBody(revision, issuedV1, validV1, 1)
	initial, err := coordinator.RefreshCatalog(context.Background())
	if err != nil || !initial.Applied {
		t.Fatalf("initial catalog must apply: %+v %v", initial, err)
	}
	if lastGood := coordinator.LastGood(); lastGood.ValidUntil != validV1 || lastGood.IssuedAt != issuedV1 {
		t.Fatalf("initial last-good wrong: %+v", lastGood)
	}

	fake.gatewayBody = sameRevisionCatalogBody(revision, issuedV2, validV2, 1)
	refreshed, err := coordinator.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.State != StateApplied || !refreshed.Applied || refreshed.Duplicate || refreshed.Catalog == nil {
		t.Fatalf("same revision newer issued_at must apply: %+v", refreshed)
	}
	if refreshed.Catalog.Revision != revision || refreshed.Catalog.ValidUntil != validV2 ||
		refreshed.Catalog.IssuedAt != issuedV2 {
		t.Fatalf("refreshed snapshot wrong: %+v", refreshed.Catalog)
	}
	if lastGood := coordinator.LastGood(); lastGood.Revision != revision ||
		lastGood.ValidUntil != validV2 || lastGood.IssuedAt != issuedV2 {
		t.Fatalf("last-good must carry the refreshed snapshot: %+v", lastGood)
	}

	duplicate, err := coordinator.RefreshCatalog(context.Background())
	if err != nil || duplicate.State != StateApplied || !duplicate.Duplicate || duplicate.Applied {
		t.Fatalf("identical refresh must stay duplicate: %+v %v", duplicate, err)
	}
}

func TestSameRevisionOlderIssuedAtCannotRollBack(t *testing.T) {
	const (
		revision = "90"
		issuedV1 = "2026-09-21T11:55:00Z"
		validV1  = "2026-09-21T12:10:00Z"
		issuedV0 = "2026-09-21T11:40:00Z"
		validV3  = "2026-09-21T13:00:00Z"
	)
	fake := &fakeServer{}
	coordinator := newSameRevisionCoordinator(t, fake)
	fake.gatewayBody = sameRevisionCatalogBody(revision, issuedV1, validV1, 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}

	// An older snapshot must not win even when its validity is longer.
	fake.gatewayBody = sameRevisionCatalogBody(revision, issuedV0, validV3, 1)
	late, err := coordinator.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if late.State != StateStale || late.Applied {
		t.Fatalf("older same-revision snapshot must be stale: %+v", late)
	}
	lastGood := coordinator.LastGood()
	if lastGood.ValidUntil != validV1 || lastGood.IssuedAt != issuedV1 {
		t.Fatalf("older snapshot rolled back last-good: %+v", lastGood)
	}
}

func TestSameRevisionShortenedValidityAppliesWithoutMax(t *testing.T) {
	const (
		revision = "90"
		issuedV1 = "2026-09-21T11:55:00Z"
		validV1  = "2026-09-21T12:10:00Z"
		issuedV2 = "2026-09-21T12:05:00Z"
		validV0  = "2026-09-21T12:06:00Z"
	)
	fake := &fakeServer{}
	coordinator := newSameRevisionCoordinator(t, fake)
	fake.gatewayBody = sameRevisionCatalogBody(revision, issuedV1, validV1, 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.gatewayBody = sameRevisionCatalogBody(revision, issuedV2, validV0, 1)
	shortened, err := coordinator.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if shortened.State != StateApplied || !shortened.Applied || shortened.Catalog == nil {
		t.Fatalf("newer same-revision snapshot with shortened validity must apply: %+v", shortened)
	}
	if shortened.Catalog.ValidUntil != validV0 {
		t.Fatalf("shortened validity must apply as-is, got %q want %q", shortened.Catalog.ValidUntil, validV0)
	}
	if lastGood := coordinator.LastGood(); lastGood.ValidUntil != validV0 || lastGood.IssuedAt != issuedV2 {
		t.Fatalf("last-good must carry the shortened snapshot: %+v", lastGood)
	}
}

func TestSameRevisionExpiredValidityStopsFailClosed(t *testing.T) {
	const (
		revision     = "90"
		issuedV1     = "2026-09-21T11:55:00Z"
		validV1      = "2026-09-21T12:10:00Z"
		issuedV2     = "2026-09-21T12:04:00Z"
		validExpired = "2026-09-21T12:00:00Z"
	)
	fake := &fakeServer{}
	coordinator := newSameRevisionCoordinator(t, fake)
	fake.gatewayBody = sameRevisionCatalogBody(revision, issuedV1, validV1, 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = sameRevisionCatalogBody(revision, issuedV2, validExpired, 1)
	expired, err := coordinator.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if expired.State != StateApplied || !expired.Applied || expired.Catalog == nil {
		t.Fatalf("newer same-revision snapshot must apply even when expired: %+v", expired)
	}

	binding := "1"
	me, err := DecodeMeStrict([]byte(meBody("7", &binding, "active")))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 5, 0, 0, time.UTC)
	decision := DecideAdmission(me, *coordinator.LastGood(), "gw-synth-0", now)
	if !decision.StopDataPlane || decision.Reason != "CATALOG_EXPIRED" {
		t.Fatalf("expired refreshed catalog must stop fail-closed: %+v", decision)
	}
}

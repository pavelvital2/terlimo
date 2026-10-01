package accountaccess

import (
	"context"
	"testing"
)

func TestPendingPollRecoveryStartsInSameCycle(t *testing.T) {
	marker := fenceOperationBody("op-1", "retryable_failure", "", `{"required":true,"catalog_revision":"20","binding_revision":"1"}`)
	fixture, server := newRecoveryRunnerServer(t, "20", marker, syncBody("op-2", "pending"))
	_, _, oldKey := newRecoveryHarness(t, fixture, server, "op-1")
	fixture.store.Value.Response.AccessApplicationState = "pending"
	runner := newRecoveryRunnerFromStore(t, fixture, server)
	if _, _, err := runner.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := fixture.eventSnapshot()
	if len(events) != 2 || events[0] != "GET /api/mobile/v1/operations/op-1?observe=sync_recovery" || events[1] != "POST /api/mobile/v1/access/sync?observe=none" {
		t.Fatalf("pending poll must hand recovery to existing coordinator within same cycle, got %v", events)
	}
	key, _, persisted := fixture.postSnapshot()
	if key == oldKey || persisted == nil || persisted.Key != key || persisted.RotationOf != "op-1" {
		t.Fatalf("recovery must remain durable and rotate only old operation")
	}
}

func TestPendingPollWithoutRecoveryNeverStartsNewEffect(t *testing.T) {
	for _, state := range []string{"pending", "applied", "rejected", "retryable_failure"} {
		t.Run(state, func(t *testing.T) {
			marker := fenceOperationBody("op-1", state, "", `{"required":false}`)
			fixture, server := newRecoveryRunnerServer(t, "20", marker, syncBody("op-2", "pending"))
			newRecoveryHarness(t, fixture, server, "op-1")
			fixture.store.Value.Response.AccessApplicationState = "pending"
			runner := newRecoveryRunnerFromStore(t, fixture, server)
			if _, _, err := runner.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			events := fixture.eventSnapshot()
			if len(events) != 1 || events[0] != "GET /api/mobile/v1/operations/op-1?observe=sync_recovery" {
				t.Fatalf("unmarked state must not POST: %v", events)
			}
		})
	}
}

package accountaccess

// Test-only regression for the ACCESS_SYNC_PENDING schedule and the fixed runner markers
// (evidence for the pending-admission analysis; no product behavior is changed here).

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

type pendingScheduleCounters struct {
	mu     sync.Mutex
	sleeps []time.Duration
	errs   []string
	stages []string
	cycles int
}

func newPendingScheduleRunner(t *testing.T) (*runnerFixture, *Runner, *pendingScheduleCounters) {
	t.Helper()
	fixture, server := newRunnerFixture(t, true)
	fixture.gatewayStatus = http.StatusConflict
	fixture.gatewayBody = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z",` +
		`"schema_version":"1.0","status":"error","code":"ACCESS_SYNC_PENDING","retryable":true,` +
		`"details":{"catalog_revision":"5","binding_revision":"1"}}`
	fixture.syncResponse = syncBody("op-1", "pending")

	session := newFixtureSession(t, fixture.auth, server)
	coordinator, err := NewCoordinator(Options{
		Client:    &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session},
		AttemptID: "attempt-schedule",
		Store:     &MemoryReceiptStore{},
		Subject:   session.Subject,
	})
	if err != nil {
		t.Fatal(err)
	}

	counters := &pendingScheduleCounters{}
	runner, err := NewRunner(RunnerConfig{
		Session:      session,
		Coordinator:  coordinator,
		Attempts:     3,
		RefreshFloor: time.Minute,
		Sleep: func(_ context.Context, d time.Duration) error {
			counters.mu.Lock()
			counters.sleeps = append(counters.sleeps, d)
			counters.mu.Unlock()
			return nil
		},
		OnError: func(code string) {
			counters.mu.Lock()
			counters.errs = append(counters.errs, code)
			counters.mu.Unlock()
		},
		SelectedNodeID: func() string { return "" },
		OnStage: func(token string, _, _ int64) {
			counters.mu.Lock()
			counters.stages = append(counters.stages, token)
			if token == "AFTER_ME_READ" {
				counters.cycles++
			}
			counters.mu.Unlock()
		},
		Emit: func(context.Context, map[string]any) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture, runner, counters
}

func TestPendingScheduleKeepsProductionRetryDelays(t *testing.T) {
	_, runner, counters := newPendingScheduleRunner(t)

	_, _, wait, attemptErr := runner.attempt(context.Background())
	if attemptErr == nil || attemptErr.Error() != "ACCESS_SYNC_PENDING" {
		t.Fatalf("err=%v, want the plain ACCESS_SYNC_PENDING sentinel", attemptErr)
	}
	if !runnerRetryable(attemptErr) {
		t.Fatalf("pending error must be retryable, got %v", attemptErr)
	}
	var status *APIStatusError
	if errors.As(attemptErr, &status) {
		t.Fatalf("pending error must not be an APIStatusError, got %+v", status)
	}
	if wait != time.Second {
		t.Fatalf("attempt-returned wait=%v, want RetryDelay(1)=1s", wait)
	}
	counters.mu.Lock()
	gotSleeps := append([]time.Duration(nil), counters.sleeps...)
	gotStages := append([]string(nil), counters.stages...)
	counters.mu.Unlock()
	if len(gotSleeps) != 2 || gotSleeps[0] != time.Second || gotSleeps[1] != 2*time.Second {
		t.Fatalf("attempt sleeps=%v, want [1s 2s]", gotSleeps)
	}
	assertPendingMarkers(t, gotStages)
}

func TestPendingScheduleRunKeepsAttemptWaitWithoutFloor(t *testing.T) {
	_, runner, counters := newPendingScheduleRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = runner.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(8 * time.Second)
	for {
		counters.mu.Lock()
		n := counters.cycles
		counters.mu.Unlock()
		if n >= 4 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("only %d cycles in 8s: a floor wait was taken instead of the attempt wait", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	counters.mu.Lock()
	finalSleeps := append([]time.Duration(nil), counters.sleeps...)
	finalErrs := append([]string(nil), counters.errs...)
	counters.mu.Unlock()
	for _, d := range finalSleeps {
		if d >= time.Minute {
			t.Fatalf("floor sleep observed: %v", finalSleeps)
		}
	}
	if len(finalErrs) == 0 {
		t.Fatal("no OnError codes captured: the runner did not complete a burst")
	}
	for _, code := range finalErrs {
		// The first burst returns the plain ACCESS_SYNC_PENDING; later bursts end at the
		// fixture-session boundary (TRANSPORT). Both are the review harness, not the
		// schedule under test.
		if code != "ACCESS_SYNC_PENDING" && code != "TRANSPORT" {
			t.Fatalf("unexpected error code %q", code)
		}
	}
}

// assertPendingMarkers checks the fixed observation tokens around the pending refresh and
// the retry scheduling, without duplicating the existing GW_PENDING_REFRESH_BEGIN marker.
func assertPendingMarkers(t *testing.T, stages []string) {
	t.Helper()
	begins, ends, sleeps, waits := 0, 0, 0, 0
	pendingOpen := false
	for _, token := range stages {
		switch token {
		case "GW_PENDING_REFRESH_BEGIN":
			if pendingOpen {
				t.Fatalf("nested pending refresh marker: %v", stages)
			}
			pendingOpen = true
			begins++
		case "GW_PENDING_REFRESH_END":
			if !pendingOpen {
				t.Fatalf("pending end without begin: %v", stages)
			}
			pendingOpen = false
			ends++
		case "ATTEMPT_RETRY_SLEEP":
			sleeps++
		case "ATTEMPT_RETRY_WAIT":
			waits++
		case "ATTEMPT_TERMINAL":
			t.Fatalf("pending class must stay retryable, not terminal: %v", stages)
		}
	}
	if begins == 0 || ends != begins {
		t.Fatalf("pending markers unbalanced: begins=%d ends=%d (%v)", begins, ends, stages)
	}
	if sleeps != 2 {
		t.Fatalf("retry-sleep markers=%d, want 2 per attempt (%v)", sleeps, stages)
	}
	if waits != 1 {
		t.Fatalf("retry-wait markers=%d, want 1 per attempt (%v)", waits, stages)
	}
}

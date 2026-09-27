package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"wg-turn-client/onboarding"
)

// TestBridgeExplicitSelectionFlag proves the host funnel flag: only a select_node
// message carrying explicit_connect arms the onboarding path, and the next select
// clears it. The bridge reader is the real one; no test-only path exists.
func TestBridgeExplicitSelectionFlag(t *testing.T) {
	b := newManagedBridge(io.Discard, "onboarding-test", func() {})
	b.read(context.Background(), bufio.NewScanner(strings.NewReader(
		`{"v":1,"attempt_id":"onboarding-test","type":"select_node","node_id":"node-a","explicit_connect":true}`+"\n")))
	if id := <-b.selection; id != "node-a" {
		t.Fatalf("selection: %q", id)
	}
	if !b.explicitSelection() {
		t.Fatal("explicit select must arm the onboarding path")
	}
	b.read(context.Background(), bufio.NewScanner(strings.NewReader(
		`{"v":1,"attempt_id":"onboarding-test","type":"select_node","node_id":"node-b"}`+"\n")))
	if id := <-b.selection; id != "node-b" {
		t.Fatalf("selection: %q", id)
	}
	if b.explicitSelection() {
		t.Fatal("a plain select must clear the onboarding path")
	}
}

// TestOnboardingDialerFailsClosedWithoutSeed keeps the real bootstrap dialer from
// inventing TURN/VK material: without the configured public service seed it refuses.
func TestOnboardingDialerFailsClosedWithoutSeed(t *testing.T) {
	dial := onboardingDialer(nil)
	conn, cleanup, err := dial(context.Background(), onboarding.GatewayEndpoint{PeerIP: "203.0.113.7", DTLSPort: 56002},
		onboarding.Bootstrap{CredentialID: "c", Secret: "s"})
	if !errors.Is(err, onboarding.ErrStartUnavailable) {
		t.Fatalf("dialer without seed: %v", err)
	}
	if conn != nil || cleanup != nil {
		t.Fatal("failed dial must return no connection and no cleanup")
	}
}

// TestOnboardingFailureTokenIsFixedAndBounded proves every explicit error normalizes to
// one fixed token: raw backend codes/statuses/transport text never appear, distinct flow
// errors keep distinct tokens, and unmapped API errors collapse to ONBOARDING_UNKNOWN.
func TestOnboardingFailureTokenIsFixedAndBounded(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"intent expired", &onboarding.APIError{HTTPStatus: 410, Code: "ONBOARDING_INTENT_EXPIRED"}, "ONBOARDING_EXPIRED"},
		{"intent revoked", &onboarding.APIError{HTTPStatus: 410, Code: "ONBOARDING_INTENT_REVOKED"}, "ONBOARDING_REVOKED"},
		{"unmapped api code", &onboarding.APIError{HTTPStatus: 409, Code: "ONBOARDING_START_CONFLICT"}, "ONBOARDING_UNKNOWN"},
		{"in flight", onboarding.ErrInFlight, "ONBOARDING_IN_FLIGHT"},
		{"cancelled sentinel", onboarding.ErrCancelled, "ONBOARDING_CANCELLED"},
		{"pending budget", onboarding.ErrPendingBudget, "ONBOARDING_PENDING_BUDGET"},
		{"start unavailable", onboarding.ErrStartUnavailable, "ONBOARDING_START_UNAVAILABLE"},
		{"start not ready", onboarding.ErrStartNotReady, "ONBOARDING_START_NOT_READY"},
		{"correlation", onboarding.ErrCorrelation, "ONBOARDING_CORRELATION"},
		{"context cancelled", context.Canceled, "ONBOARDING_CANCELLED"},
		{"context deadline", context.DeadlineExceeded, "ONBOARDING_TIMEOUT"},
		{"raw error", errors.New("raw transport text"), "ONBOARDING_FAILED"},
		{"nil", nil, "ONBOARDING_UNKNOWN"},
	}
	for _, tc := range cases {
		got := onboardingFailureToken(tc.err)
		if got != tc.want {
			t.Fatalf("%s: token = %q, want %q", tc.name, got, tc.want)
		}
		if !onboardingTokenSet[got] {
			t.Fatalf("%s: token %q is not in the fixed vocabulary", tc.name, got)
		}
		if strings.ContainsAny(got, " :/") || strings.Contains(got, "HTTP") {
			t.Fatalf("%s: token %q is not a fixed identifier", tc.name, got)
		}
	}
}

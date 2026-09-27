package main

// Focused diagnostics tests for the explicit onboarding-hour path: every emitted line is
// a fixed `onboarding: TOKEN`, exactly one ENTRY and one terminal per explicitConnect
// call survive repeated pending/retry polls, and no raw backend code/status is ever
// written. Behavior (calls, error identity) is asserted unchanged alongside.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"wg-turn-client/onboarding"
)

type diagIntentClient struct {
	calls int
	step  func(n int, requestKey string) (onboarding.IntentPoll, *onboarding.APIError, error)
}

func (d *diagIntentClient) Intent(_ context.Context, requestKey, _ string) (onboarding.IntentPoll, *onboarding.APIError, error) {
	d.calls++
	return d.step(d.calls, requestKey)
}

type diagStarter struct {
	calls int
	reply onboarding.StartReply
	err   error
}

func (d *diagStarter) Start(context.Context, onboarding.IntentPoll) (onboarding.StartReply, *onboarding.APIError, error) {
	d.calls++
	return d.reply, nil, d.err
}

func captureOnboardingDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	buffer := &bytes.Buffer{}
	previous := onboardingDiagnostics
	onboardingDiagnostics = buffer
	t.Cleanup(func() { onboardingDiagnostics = previous })
	return buffer
}

func diagLines(buffer *bytes.Buffer) []string {
	raw := strings.TrimSuffix(buffer.String(), "\n")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

func diagFlow(t *testing.T, intents onboarding.IntentClient, starter onboarding.Starter, options onboarding.Options) *onboarding.Flow {
	t.Helper()
	if options.Sleep == nil {
		options.Sleep = func(context.Context, time.Duration) error { return nil }
	}
	flow, err := onboarding.NewFlow(&onboarding.MemoryStore{}, intents, starter, options)
	if err != nil {
		t.Fatalf("flow: %v", err)
	}
	return flow
}

// TestExplicitConnectPendingRetriesKeepEntryAndTerminalOnce proves the bounded stage
// policy: many pending polls of one explicit run add no diagnostic lines, and the single
// terminal token still lands.
func TestExplicitConnectPendingRetriesKeepEntryAndTerminalOnce(t *testing.T) {
	intents := &diagIntentClient{step: func(_ int, requestKey string) (onboarding.IntentPoll, *onboarding.APIError, error) {
		return onboarding.IntentPoll{State: onboarding.StatePending, IntentID: "intent-diag", RequestKey: requestKey}, nil, nil
	}}
	flow := diagFlow(t, intents, nil, onboarding.Options{MaxPendingPolls: 5})
	mobile := &managedMobile{explicit: flow}
	diag := captureOnboardingDiagnostics(t)

	_, err := mobile.explicitConnect(context.Background(), "")
	if !errors.Is(err, onboarding.ErrPendingBudget) {
		t.Fatalf("pending budget error changed: %v", err)
	}
	if intents.calls != 5 {
		t.Fatalf("pending polls = %d, want 5", intents.calls)
	}
	want := []string{"onboarding: ENTRY", "onboarding: ONBOARDING_PENDING_BUDGET"}
	if got := diagLines(diag); !equalLines(got, want) {
		t.Fatalf("diagnostics = %q, want %q", got, want)
	}
}

// TestExplicitConnectStartSuccessEmitsEntryStartTerminalOnce pins the success vocabulary.
func TestExplicitConnectStartSuccessEmitsEntryStartTerminalOnce(t *testing.T) {
	const intentID = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	intents := &diagIntentClient{step: func(_ int, requestKey string) (onboarding.IntentPoll, *onboarding.APIError, error) {
		return onboarding.IntentPoll{State: onboarding.StateReady, IntentID: intentID, RequestKey: requestKey,
			StartChallenge: &onboarding.StartChallenge{ChallengeID: "00112233445566778899aabbccddeeff", NonceB64: "bm9uY2U"}}, nil, nil
	}}
	starter := &diagStarter{reply: onboarding.StartReply{IntentID: intentID}}
	flow := diagFlow(t, intents, starter, onboarding.Options{})
	mobile := &managedMobile{explicit: flow}
	diag := captureOnboardingDiagnostics(t)

	if _, err := mobile.explicitConnect(context.Background(), ""); err != nil {
		t.Fatalf("explicit connect: %v", err)
	}
	if starter.calls != 1 || intents.calls != 1 {
		t.Fatalf("start calls = %d, intent calls = %d", starter.calls, intents.calls)
	}
	want := []string{"onboarding: ENTRY", "onboarding: START_ENTRY", "onboarding: STARTED"}
	if got := diagLines(diag); !equalLines(got, want) {
		t.Fatalf("diagnostics = %q, want %q", got, want)
	}
}

// TestExplicitConnectTerminalStatesEmitFixedTokens proves recovered/terminal flow states
// keep their external meaning: a recovered started hour returns success, a failed intent
// returns success-with-terminal-state, and expired/revoked API errors keep returning the
// exact error while stderr reports only the fixed terminal token.
func TestExplicitConnectTerminalStatesEmitFixedTokens(t *testing.T) {
	pollCases := []struct {
		state onboarding.State
		want  string
	}{
		{onboarding.StateStarted, "onboarding: STARTED"},
		{onboarding.StateFailed, "onboarding: ONBOARDING_FAILED"},
	}
	for _, tc := range pollCases {
		intents := &diagIntentClient{step: func(_ int, requestKey string) (onboarding.IntentPoll, *onboarding.APIError, error) {
			return onboarding.IntentPoll{State: tc.state, IntentID: "intent-diag", RequestKey: requestKey}, nil, nil
		}}
		flow := diagFlow(t, intents, nil, onboarding.Options{})
		mobile := &managedMobile{explicit: flow}
		diag := captureOnboardingDiagnostics(t)
		if _, err := mobile.explicitConnect(context.Background(), ""); err != nil {
			t.Fatalf("%s: explicit connect: %v", tc.state, err)
		}
		want := []string{"onboarding: ENTRY", tc.want}
		if got := diagLines(diag); !equalLines(got, want) {
			t.Fatalf("%s: diagnostics = %q, want %q", tc.state, got, want)
		}
	}

	apiCases := []struct {
		code string
		want string
	}{
		{"ONBOARDING_INTENT_EXPIRED", "onboarding: ONBOARDING_EXPIRED"},
		{"ONBOARDING_INTENT_REVOKED", "onboarding: ONBOARDING_REVOKED"},
	}
	for _, tc := range apiCases {
		intents := &diagIntentClient{step: func(_ int, _ string) (onboarding.IntentPoll, *onboarding.APIError, error) {
			return onboarding.IntentPoll{}, &onboarding.APIError{HTTPStatus: 410, Code: tc.code}, nil
		}}
		flow := diagFlow(t, intents, nil, onboarding.Options{})
		mobile := &managedMobile{explicit: flow}
		diag := captureOnboardingDiagnostics(t)
		_, err := mobile.explicitConnect(context.Background(), "")
		var apiError *onboarding.APIError
		if !errors.As(err, &apiError) || apiError.Code != tc.code {
			t.Fatalf("%s: error identity changed: %v", tc.code, err)
		}
		want := []string{"onboarding: ENTRY", tc.want}
		if got := diagLines(diag); !equalLines(got, want) {
			t.Fatalf("%s: diagnostics = %q, want %q", tc.code, got, want)
		}
	}
}

// TestExplicitConnectNeverEmitsRawBackendCodes proves the decodable backend error keeps
// its returned error identity while stderr carries only ONBOARDING_UNKNOWN.
func TestExplicitConnectNeverEmitsRawBackendCodes(t *testing.T) {
	intents := &diagIntentClient{step: func(_ int, _ string) (onboarding.IntentPoll, *onboarding.APIError, error) {
		return onboarding.IntentPoll{}, &onboarding.APIError{HTTPStatus: 409, Code: "ONBOARDING_START_CONFLICT"}, nil
	}}
	flow := diagFlow(t, intents, nil, onboarding.Options{})
	mobile := &managedMobile{explicit: flow}
	diag := captureOnboardingDiagnostics(t)

	_, err := mobile.explicitConnect(context.Background(), "")
	var apiError *onboarding.APIError
	if !errors.As(err, &apiError) || apiError.Code != "ONBOARDING_START_CONFLICT" {
		t.Fatalf("returned error identity changed: %v", err)
	}
	want := []string{"onboarding: ENTRY", "onboarding: ONBOARDING_UNKNOWN"}
	if got := diagLines(diag); !equalLines(got, want) {
		t.Fatalf("diagnostics = %q, want %q", got, want)
	}
	if strings.Contains(diag.String(), "HTTP409") || strings.Contains(diag.String(), "CONFLICT") {
		t.Fatalf("raw backend code leaked: %q", diag.String())
	}
}

// TestOnboardingIntentStageTokensAreFixed proves the stage mapping stays inside the
// fixed vocabulary; an unknown stage name can never reach stderr.
func TestOnboardingIntentStageTokensAreFixed(t *testing.T) {
	if len(onboardingIntentStageTokens) != 10 {
		t.Fatalf("intent stage count = %d, want 10", len(onboardingIntentStageTokens))
	}
	for _, stage := range []string{"challenge_transport", "challenge_status_4xx", "challenge_status_5xx",
		"challenge_status_other", "challenge_decode", "challenge_semantic"} {
		token, ok := onboardingIntentStageTokens[stage]
		if !ok || !onboardingTokenSet[token] {
			t.Fatalf("challenge stage %q missing or outside the vocabulary: %q", stage, token)
		}
	}
	for stage, token := range onboardingIntentStageTokens {
		if !onboardingTokenSet[token] {
			t.Fatalf("stage %q token %q outside the vocabulary", stage, token)
		}
	}
	diag := captureOnboardingDiagnostics(t)
	emitOnboardingToken("HTTP409 ONBOARDING_START_CONFLICT")
	if got := diagLines(diag); !equalLines(got, []string{"onboarding: ONBOARDING_UNKNOWN"}) {
		t.Fatalf("unknown token must collapse: %q", got)
	}
}

func equalLines(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

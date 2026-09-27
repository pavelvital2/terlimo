package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"wg-turn-client/accountaccess"
)

func browseFor(validUntil string) accountaccess.BrowseCatalogResponse {
	return accountaccess.BrowseCatalogResponse{
		Status: "ok", CatalogMode: "browse", ValidUntil: validUntil,
		Gateways: []accountaccess.BrowseGateway{{GatewayID: "gw-1", Name: "node"}},
	}
}

type stubOnboardingMobile struct {
	activated  bool
	connectErr error
	waitErr    error
	awaitCalls int
	// childExpiry mirrors a stub whose wait observes its own ctx.
	childExpiry bool
}

func (s *stubOnboardingMobile) explicitConnect(context.Context, string) (bool, error) {
	return s.activated, s.connectErr
}

func (s *stubOnboardingMobile) awaitOnboardingRefresh(ctx context.Context) error {
	s.awaitCalls++
	if s.childExpiry {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.waitErr
}

// Caller path (the exact branch waitForNode uses): an optional onboarding-hour API
// error must stay diagnostic-only and never block the accepted admission/selection.
func TestRunExplicitOnboardingOptionalErrorDoesNotBlockAdmission(t *testing.T) {
	stub := &stubOnboardingMobile{activated: false, connectErr: errors.New("ONBOARDING_START_NOT_READY")}
	c := &managedController{}
	if err := c.runExplicitOnboarding(context.Background(), context.Background(), "gw", stub); err != nil {
		t.Fatalf("optional error must be diagnostic-only: %v", err)
	}
	if stub.awaitCalls != 0 {
		t.Fatalf("non-activated attempt must not wait: %d", stub.awaitCalls)
	}
}

// A canceled parent is an honest failure, even when the optional error is present.
func TestRunExplicitOnboardingParentCanceledIsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stub := &stubOnboardingMobile{connectErr: errors.New("ONBOARDING_START_NOT_READY")}
	if err := (&managedController{}).runExplicitOnboarding(ctx, context.Background(), "gw", stub); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled parent must fail: %v", err)
	}
}

// Activated + fresh browse returns the fixed explicit error from the wait.
func TestRunExplicitOnboardingActivatedBrowseReturnsFixedError(t *testing.T) {
	stub := &stubOnboardingMobile{activated: true, waitErr: errors.New("ONBOARDING_CREDENTIAL_UNAVAILABLE")}
	err := (&managedController{}).runExplicitOnboarding(context.Background(), context.Background(), "gw", stub)
	if err == nil || !strings.Contains(err.Error(), "ONBOARDING_CREDENTIAL_UNAVAILABLE") {
		t.Fatalf("browse-only must fail explicitly: %v", err)
	}
	if stub.awaitCalls != 1 {
		t.Fatalf("activated attempt must wait exactly once: %d", stub.awaitCalls)
	}
}

// Activated + verified refresh continues the same single Connect (caller continues
// to chooseNode with a nil error; no second user tap).
func TestRunExplicitOnboardingActivatedVerifiedContinues(t *testing.T) {
	stub := &stubOnboardingMobile{activated: true, waitErr: nil}
	if err := (&managedController{}).runExplicitOnboarding(context.Background(), context.Background(), "gw", stub); err != nil {
		t.Fatalf("verified must continue: %v", err)
	}
	if stub.awaitCalls != 1 {
		t.Fatalf("want one post-start wait: %d", stub.awaitCalls)
	}
}

// failed/expired/revoked => (false,nil): no wait, normal path continues.
func TestRunExplicitOnboardingNotActivatedNoWait(t *testing.T) {
	stub := &stubOnboardingMobile{activated: false, connectErr: nil}
	if err := (&managedController{}).runExplicitOnboarding(context.Background(), context.Background(), "gw", stub); err != nil || stub.awaitCalls != 0 {
		t.Fatalf("non-activated must not wait: err=%v calls=%d", err, stub.awaitCalls)
	}
}

// The post-start wait completes on a browse callback (identical/deduped included)
// with the fixed error and the terminal token.
func TestOnboardingAwaitBrowseCompletesWithExplicitError(t *testing.T) {
	m := &managedMobile{browseSig: make(chan struct{}, 1)}
	m.onBrowse(browseFor("t1")) // prime dedup; browseGen=1
	diag := captureOnboardingDiagnostics(t)
	done := make(chan error, 1)
	go func() { done <- m.awaitOnboardingRefresh(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	m.onBrowse(browseFor("t1")) // identical -> deduped but must still signal
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "ONBOARDING_CREDENTIAL_UNAVAILABLE") {
			t.Fatalf("browse-only must fail explicitly: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("identical browse did not complete the wait")
	}
	if !strings.Contains(diag.String(), "onboarding: ONBOARDING_CREDENTIAL_UNAVAILABLE") {
		t.Fatalf("fixed terminal token missing: %q", diag.String())
	}
}

// Verified refresh completes the wait successfully.
func TestOnboardingAwaitVerifiedContinues(t *testing.T) {
	m := &managedMobile{verifiedSig: make(chan struct{}, 1), browseSig: make(chan struct{}, 1)}
	done := make(chan error, 1)
	go func() { done <- m.awaitOnboardingRefresh(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	m.mu.Lock()
	m.verified++
	sig := m.verifiedSig
	m.mu.Unlock()
	sig <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("verified must continue: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("verified did not complete the wait")
	}
}

// A canceled parent is never a success, and an already-expired parent is a failure.
func TestOnboardingAwaitExpiredParentIsFailure(t *testing.T) {
	m := &managedMobile{verifiedSig: make(chan struct{}, 1), browseSig: make(chan struct{}, 1)}
	m.verified = 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.awaitOnboardingRefresh(ctx); err == nil {
		t.Fatal("expired parent must not be reported as success")
	}
}

// The exact production caller-context split: a live parent with an expired child
// 15s budget keeps the optional onboarding error diagnostic-only (admission path
// continues); a canceled parent is a failure; activated + expired child is terminal.
func TestRunExplicitOnboardingParentAliveChildExpiredOptionalError(t *testing.T) {
	parent := context.Background()
	child, cancelChild := context.WithTimeout(parent, time.Nanosecond)
	defer cancelChild()
	time.Sleep(5 * time.Millisecond) // child is now expired, parent alive
	stub := &stubOnboardingMobile{connectErr: errors.New("ONBOARDING_START_NOT_READY")}
	if err := (&managedController{}).runExplicitOnboarding(parent, child, "gw", stub); err != nil {
		t.Fatalf("optional error under live parent must stay diagnostic-only: %v", err)
	}
}

func TestRunExplicitOnboardingCanceledParentWithLiveChild(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	stub := &stubOnboardingMobile{connectErr: errors.New("ONBOARDING_START_NOT_READY")}
	if err := (&managedController{}).runExplicitOnboarding(parent, context.Background(), "gw", stub); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled parent must fail even with a live child: %v", err)
	}
}

func TestRunExplicitOnboardingActivatedChildExpiredIsTerminal(t *testing.T) {
	child, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond)
	stub := &stubOnboardingMobile{activated: true, childExpiry: true}
	if err := (&managedController{}).runExplicitOnboarding(context.Background(), child, "gw", stub); err == nil {
		t.Fatal("activated post-start wait deadline must be terminal")
	}
}

package main

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestCancelSourceFirstWinsPerAttempt(t *testing.T) {
	d := &managedDiagnostics{}
	d.noteCancelSource(vpnCancelHostStop)
	d.noteCancelSource(vpnCancelRevoked)
	if got := d.cancelSourceOrUnknown(); got != vpnCancelHostStop {
		t.Fatalf("first cause overwritten: %s", got)
	}
}

// Real production seams: the SWITCH/REVOKED callbacks must mark the attempt diagnostics
// captured by their own attempt, even after c.vpnDiagnostics was replaced by a new one.
func TestOldAttemptCallbacksCannotMarkReplacedAttempt(t *testing.T) {
	oldDiag := &managedDiagnostics{}
	newDiag := &managedDiagnostics{}
	c := &managedController{}
	c.vpnDiagnostics = oldDiag
	c.vpnDiagnostics = newDiag // a new Connect replaced the current attempt

	noteSwitchCancelSource(oldDiag, func() {})   // async old-attempt SWITCH callback
	noteRevokedCancelSource(oldDiag, func() {})  // late old REVOKED callback
	if got := oldDiag.cancelSourceOrUnknown(); got != vpnCancelSwitch {
		t.Fatalf("old attempt first cause: %s", got)
	}
	if got := newDiag.cancelSourceOrUnknown(); got != vpnCancelUnknown {
		t.Fatalf("new attempt was polluted by old callback: %s", got)
	}
	// The new attempt then records its own cause independently.
	noteRevokedCancelSource(newDiag, func() {})
	if got := newDiag.cancelSourceOrUnknown(); got != vpnCancelRevoked {
		t.Fatalf("new attempt own cause: %s", got)
	}
}

// Drives the actual production terminal decision (awaitManagedSetup) instead of emitting
// directly, and asserts exactly one CANCEL_SOURCE line.
func TestAwaitManagedSetupTerminalMarkerExactlyOnce(t *testing.T) {
	var buf bytes.Buffer
	oldOut := diagStageDiagnostics
	diagStageDiagnostics = &buf
	defer func() { diagStageDiagnostics = oldOut }()

	// First cause recorded, then ctx cancelled with a live parent -> one CONNECT_BUDGET line.
	d := &managedDiagnostics{}
	d.noteCancelSource(vpnCancelConnectBudget)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan := managedTunnelPlan{Diagnostics: d, Parent: context.Background()}
	if _, err := awaitManagedSetup(ctx, plan, make(chan string), make(chan time.Time)); err == nil || err.Error() != "VPN_SETUP_FAILED" {
		t.Fatalf("want VPN_SETUP_FAILED, got %v", err)
	}
	if got, want := buf.String(), "vpnstage: CANCEL_SOURCE CONNECT_BUDGET\n"; got != want {
		t.Fatalf("terminal marker wrong:\n got %q\nwant %q", got, want)
	}

	// Unset first cause -> exactly one UNKNOWN line, no more.
	buf.Reset()
	d2 := &managedDiagnostics{}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	plan2 := managedTunnelPlan{Diagnostics: d2, Parent: context.Background()}
	_, _ = awaitManagedSetup(ctx2, plan2, make(chan string), make(chan time.Time))
	if got, want := buf.String(), "vpnstage: CANCEL_SOURCE UNKNOWN\n"; got != want {
		t.Fatalf("unset marker wrong: %q", got)
	}

	// Parent already cancelled: returns the parent error and emits NO marker.
	buf.Reset()
	d3 := &managedDiagnostics{}
	parentCtx, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	ctx3, cancel3 := context.WithCancel(context.Background())
	cancel3()
	plan3 := managedTunnelPlan{Diagnostics: d3, Parent: parentCtx}
	if _, err := awaitManagedSetup(ctx3, plan3, make(chan string), make(chan time.Time)); err == nil {
		t.Fatal("want parent error")
	}
	if buf.Len() != 0 {
		t.Fatalf("parent-cancel path emitted a marker: %q", buf.String())
	}

	// Success path: config delivered -> no marker.
	buf.Reset()
	d4 := &managedDiagnostics{}
	cfgCh := make(chan string, 1)
	cfgCh <- "cfg"
	plan4 := managedTunnelPlan{Diagnostics: d4, Parent: context.Background()}
	if raw, err := awaitManagedSetup(context.Background(), plan4, cfgCh, make(chan time.Time)); err != nil || raw != "cfg" {
		t.Fatalf("success path: %q %v", raw, err)
	}
	if buf.Len() != 0 {
		t.Fatalf("success path emitted a marker: %q", buf.String())
	}
}

// TestCancelManagedPreparationNoteBeforeCancel proves note-before-cancel ordering through
// the real AfterFunc path, synchronised by a channel (no Sleep-only sync).
func TestCancelManagedPreparationNoteBeforeCancel(t *testing.T) {
	cancelled := make(chan string, 1)
	d := &managedDiagnostics{}
	prep, cancelPrep := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancelPrep()
	stop := cancelManagedPreparation(prep, func() { cancelled <- d.cancelSourceOrUnknown() }, d, true)
	select {
	case src := <-cancelled:
		if src != vpnCancelConnectBudget {
			t.Fatalf("want CONNECT_BUDGET before cancel, got %s", src)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel not invoked")
	}
	stop()

	cancelled2 := make(chan string, 1)
	d2 := &managedDiagnostics{}
	parent, cancelParent := context.WithCancel(context.Background())
	prep2, cancelPrep2 := context.WithCancel(parent)
	defer cancelPrep2()
	stop2 := cancelManagedPreparation(prep2, func() { cancelled2 <- d2.cancelSourceOrUnknown() }, d2, true)
	cancelParent()
	select {
	case src := <-cancelled2:
		if src != vpnCancelParentShutdown {
			t.Fatalf("want PARENT_SHUTDOWN, got %s", src)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parent cancel not invoked")
	}
	stop2()
}

func TestHostStopRecordsOnItsOwnAttempt(t *testing.T) {
	first := &managedDiagnostics{}
	first.noteCancelSource(vpnCancelRevoked)
	second := &managedDiagnostics{}
	c := &managedController{mobileStops: map[uint64]mobileStopEntry{}}
	r1 := c.registerMobileStop(func() {}, first)
	r2 := c.registerMobileStop(func() {}, second)
	c.stopMobileDataPlane()
	if got := second.cancelSourceOrUnknown(); got != vpnCancelHostStop {
		t.Fatalf("second source: %s", got)
	}
	if got := first.cancelSourceOrUnknown(); got != vpnCancelRevoked {
		t.Fatalf("first overwritten: %s", got)
	}
	r1()
	r2()
}


func TestAwaitManagedSetupSynchronousPreparationFallback(t *testing.T) {
	// Parent-cancel observed synchronously (AfterFunc race removed): PARENT_SHUTDOWN.
	d := &managedDiagnostics{}
	parent, cancelParent := context.WithCancel(context.Background())
	prep, cancelPrep := context.WithCancel(parent)
	defer cancelPrep()
	cancelParent()
	ctx, cancelCtx := context.WithCancel(context.Background())
	cancelCtx()
	plan := managedTunnelPlan{Diagnostics: d, Parent: context.Background(), Preparation: prep}
	if _, err := awaitManagedSetup(ctx, plan, make(chan string), make(chan time.Time)); err == nil || err.Error() != "VPN_SETUP_FAILED" {
		t.Fatalf("want VPN_SETUP_FAILED, got %v", err)
	}
	if got := d.cancelSourceOrUnknown(); got != vpnCancelParentShutdown {
		t.Fatalf("want PARENT_SHUTDOWN, got %s", got)
	}

	// Expired preparation deadline -> CONNECT_BUDGET.
	d2 := &managedDiagnostics{}
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	ctx2, cancelCtx2 := context.WithCancel(context.Background())
	cancelCtx2()
	plan2 := managedTunnelPlan{Diagnostics: d2, Parent: context.Background(), Preparation: expired}
	_, _ = awaitManagedSetup(ctx2, plan2, make(chan string), make(chan time.Time))
	if got := d2.cancelSourceOrUnknown(); got != vpnCancelConnectBudget {
		t.Fatalf("want CONNECT_BUDGET, got %s", got)
	}

	// A worker cause recorded first-wins is not overwritten by the fallback.
	d3 := &managedDiagnostics{}
	d3.noteCancelSource(vpnCancelWorkerCreds)
	parent3, cancelParent3 := context.WithCancel(context.Background())
	prep3, cancelPrep3 := context.WithCancel(parent3)
	defer cancelPrep3()
	cancelParent3()
	ctx3, cancelCtx3 := context.WithCancel(context.Background())
	cancelCtx3()
	plan3 := managedTunnelPlan{Diagnostics: d3, Parent: context.Background(), Preparation: prep3}
	_, _ = awaitManagedSetup(ctx3, plan3, make(chan string), make(chan time.Time))
	if got := d3.cancelSourceOrUnknown(); got != vpnCancelWorkerCreds {
		t.Fatalf("worker cause overwritten: %s", got)
	}
}

// Baseline check (manual, documented): on 69b4aa5 the synchronous fallback is absent, so
// TestAwaitManagedSetupSynchronousPreparationFallback would read UNKNOWN — this test is the
// regression that fails on the rejected baseline.

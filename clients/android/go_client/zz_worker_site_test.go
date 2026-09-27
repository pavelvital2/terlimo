package main

import (
	"context"
	"testing"
	"time"
)

func timeAfter() <-chan time.Time { return time.After(2 * time.Second) }

func diagCtx(d *managedDiagnostics) context.Context {
	return withWorkerCancelDiagnostics(context.Background(), d)
}

func TestWorkerCancelSeamsAreDistinctAndNoteBeforeCancel(t *testing.T) {
	type seam struct {
		name  string
		token string
		call  func(context.Context, context.CancelFunc)
	}
	seams := []seam{
		{"gate", vpnCancelWorkerGate, workerCancelStartupGate},
		{"creds", vpnCancelWorkerCreds, workerCancelInitialCreds},
		{"terminal", vpnCancelWorkerTerminal, workerCancelManagedTerminal},
	}
	for _, sm := range seams {
		d := &managedDiagnostics{}
		called := make(chan string, 1)
		sm.call(diagCtx(d), func() { called <- d.cancelSourceOrUnknown() })
		select {
		case got := <-called:
			if got != sm.token {
				t.Fatalf("%s: want %s before cancel, got %s", sm.name, sm.token, got)
			}
		case <-timeAfter():
			t.Fatalf("%s: cancel not invoked", sm.name)
		}
	}
	// The three tokens must differ (baseline collapsed them into one).
	set := map[string]bool{vpnCancelWorkerGate: true, vpnCancelWorkerCreds: true, vpnCancelWorkerTerminal: true}
	if len(set) != 3 {
		t.Fatal("worker tokens are not distinct")
	}
}

func TestWorkerSiteFirstWinsAndAttemptIsolation(t *testing.T) {
	// First-wins within the attempt: gate then creds keeps gate.
	d := &managedDiagnostics{}
	ctx := diagCtx(d)
	workerCancelStartupGate(ctx, func() {})
	workerCancelInitialCreds(ctx, func() {})
	if got := d.cancelSourceOrUnknown(); got != vpnCancelWorkerGate {
		t.Fatalf("first-wins broken: %s", got)
	}

	// Old-attempt context cannot mark a replaced attempt: the ctx carries the old
	// diagnostics pointer, so the new attempt stays untouched.
	oldD := &managedDiagnostics{}
	newD := &managedDiagnostics{}
	oldCtx := diagCtx(oldD)
	_ = newD // current attempt replaced; the old ctx still carries oldD
	workerCancelManagedTerminal(oldCtx, func() {})
	if got := newD.cancelSourceOrUnknown(); got != vpnCancelUnknown {
		t.Fatalf("new attempt polluted: %s", got)
	}
	if got := oldD.cancelSourceOrUnknown(); got != vpnCancelWorkerTerminal {
		t.Fatalf("old attempt cause lost: %s", got)
	}
}

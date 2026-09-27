package main

import (
	"context"
	"errors"
	"testing"
)

func TestManagedVPNSetupFreezeWorkerOrdering(t *testing.T) {
	for _, workerFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "worker-before-timeout", false: "worker-after-timeout"}[workerFirst], func(t *testing.T) {
			d := &managedDiagnostics{}
			d.noteVPN(vpnStageHandshake, nil)
			firstDone, allDone := make(chan struct{}), make(chan struct{})
			worker := func() { d.noteVPN(vpnStageAuth, errors.New("LEASE_EXPIRED")) }
			freeze := func() { d.freezeVPNAtSetupTimeout() }
			first, second := freeze, worker
			if workerFirst {
				first, second = worker, freeze
			}
			go func() { first(); close(firstDone) }()
			go func() { <-firstDone; second(); close(allDone) }()
			<-allDone
			d.noteVPNCancelIfNoFailure(vpnStageConfig, context.Canceled)
			d.noteVPN(vpnStageHandshake, context.Canceled)
			d.noteVPN(vpnStageConfig, nil)
			stage, failure, code := d.vpnSnapshot()
			wantStage, wantFailure, wantCode := "HANDSHAKE", "NONE", "NONE"
			if workerFirst {
				wantStage, wantFailure, wantCode = "AUTH", "FAILED", "LEASE_EXPIRED"
			}
			if stage != wantStage || failure != wantFailure || code != wantCode {
				t.Fatalf("frozen evidence = %s/%s/%s, want %s/%s/%s", stage, failure, code, wantStage, wantFailure, wantCode)
			}
		})
	}
}

func TestManagedVPNSetupFreezePendingAndRetry(t *testing.T) {
	d := &managedDiagnostics{}
	d.noteVPN(vpnStageHandshake, context.DeadlineExceeded)
	d.noteVPN(vpnStageHandshake, nil) // Legitimate retry before aggregate timeout.
	d.freezeVPNAtSetupTimeout()
	if stage, failure, _ := d.vpnSnapshot(); stage != "HANDSHAKE" || failure != "NONE" {
		t.Fatalf("retry observation lost: %s/%s", stage, failure)
	}
	pending := &managedDiagnostics{}
	pending.freezeVPNAtSetupTimeout()
	pending.noteVPN(vpnStageHandshake, nil)
	if stage, failure, _ := pending.vpnSnapshot(); stage != "PENDING" || failure != "NONE" {
		t.Fatalf("unstarted worker observation fabricated: %s/%s", stage, failure)
	}
}

func TestManagedVPNSetupFreezeActualWorkerStages(t *testing.T) {
	for _, tc := range []struct {
		stage managedVPNStage
		name  string
	}{{vpnStageHandshake, "HANDSHAKE"}, {vpnStageAuth, "AUTH"}, {vpnStageConfig, "CONFIG"}} {
		t.Run(tc.name, func(t *testing.T) {
			d := &managedDiagnostics{}
			d.noteVPN(tc.stage, nil)
			d.freezeVPNAtSetupTimeout()
			d.noteVPNCancelIfNoFailure(vpnStageConfig, context.Canceled)
			d.noteVPN(vpnStageAuth, errors.New("LEASE_EXPIRED"))
			stage, failure, code := d.vpnSnapshot()
			if stage != tc.name || failure != "NONE" || code != "NONE" {
				t.Fatalf("last real worker stage changed: %s/%s/%s", stage, failure, code)
			}
		})
	}
}

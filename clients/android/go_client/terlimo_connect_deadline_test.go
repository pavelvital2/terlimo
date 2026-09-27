package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestManagedConnectDeadlineCancelsWorkersAfterFreeze(t *testing.T) {
	preparation, stopPreparation := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stopPreparation()
	runtime, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &managedDiagnostics{}
	d.noteVPN(vpnStageAuth, errors.New("LEASE_EXPIRED"))
	stop := cancelManagedPreparation(preparation, cancel, d, true)
	defer stop()
	done := make(chan struct{})
	go func() {
		<-runtime.Done()
		d.noteVPN(vpnStageConfig, context.Canceled)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker survived preparation timeout")
	}
	stage, failure, code := d.vpnSnapshot()
	if stage != "AUTH" || failure != "FAILED" || code != "LEASE_EXPIRED" {
		t.Fatalf("cleanup overwrote evidence: %s/%s/%s", stage, failure, code)
	}
}

func TestManagedConnectReadyDisarmsPreparation(t *testing.T) {
	preparation, stopPreparation := context.WithCancel(context.Background())
	runtime, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := cancelManagedPreparation(preparation, cancel, &managedDiagnostics{}, true)
	if !stop() {
		t.Fatal("preparation stopped before readiness")
	}
	stopPreparation()
	if runtime.Err() != nil {
		t.Fatal("ready runtime inherited preparation deadline")
	}
}

package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func wakeTestSlot() *WorkerSlot {
	return &WorkerSlot{WakeCh: make(chan uint64, 1), SleepCh: make(chan struct{}, 1), WakeAckCh: make(chan uint64, 1)}
}

func TestWorkerWakeProbePreservesResponsiveChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slot := wakeTestSlot()
	writes := make(chan struct{}, 8)
	expired := make(chan uint64, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runWorkerWakeProbeLoop(ctx, slot, func(time.Time) bool { writes <- struct{}{}; return true }, 80*time.Millisecond, 15*time.Millisecond, func(g uint64) { expired <- g })
	}()
	slot.WakeCh <- 7
	select {
	case <-writes:
	case <-time.After(time.Second):
		t.Fatal("wake probe not sent")
	}
	slot.WakeAckCh <- 7
	time.Sleep(120 * time.Millisecond)
	select {
	case g := <-expired:
		t.Fatalf("responsive worker expired %d", g)
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("probe loop survived stop")
	}
}

func TestWorkerWakeProbeExpiresOnlyUnresponsiveChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slot := wakeTestSlot()
	var writes atomic.Int32
	expired := make(chan uint64, 1)
	go runWorkerWakeProbeLoop(ctx, slot, func(time.Time) bool { writes.Add(1); return true }, 80*time.Millisecond, 15*time.Millisecond, func(g uint64) { expired <- g })
	slot.WakeCh <- 11
	select {
	case g := <-expired:
		if g != 11 {
			t.Fatalf("generation %d", g)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not expire")
	}
	if writes.Load() < 2 {
		t.Fatal("wake retries missing")
	}
}

func TestWorkerWakeProbeCancelledBySleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slot := wakeTestSlot()
	writes := make(chan struct{}, 8)
	expired := make(chan uint64, 1)
	go runWorkerWakeProbeLoop(ctx, slot, func(time.Time) bool { writes <- struct{}{}; return true }, 80*time.Millisecond, 15*time.Millisecond, func(g uint64) { expired <- g })
	slot.WakeCh <- 3
	<-writes
	slot.SleepCh <- struct{}{}
	time.Sleep(120 * time.Millisecond)
	select {
	case g := <-expired:
		t.Fatalf("sleep expired generation %d", g)
	default:
	}
}

func TestPendingSleepCannotCancelNewWakeGeneration(t *testing.T) {
	d := &Dispatcher{}
	slot := wakeTestSlot()
	workers := []*WorkerSlot{slot}
	d.workers.Store(&workers)
	d.noteDeviceSleep()
	generation := d.noteDeviceWake(time.Unix(1, 0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writes := make(chan struct{}, 8)
	expired := make(chan uint64, 1)
	go runWorkerWakeProbeLoop(ctx, slot, func(time.Time) bool {
		writes <- struct{}{}
		return true
	}, 80*time.Millisecond, 15*time.Millisecond, func(g uint64) { expired <- g })

	select {
	case <-writes:
	case <-time.After(time.Second):
		t.Fatal("new wake probe was cancelled by pending sleep")
	}
	slot.WakeAckCh <- generation
	time.Sleep(120 * time.Millisecond)
	select {
	case g := <-expired:
		t.Fatalf("new wake generation expired %d", g)
	default:
	}
}

func TestWakeGenerationEligibilityAndCurrentRegistration(t *testing.T) {
	d := &Dispatcher{}
	existing := wakeTestSlot()
	workers := []*WorkerSlot{existing}
	d.workers.Store(&workers)
	g := d.noteDeviceWake(time.Unix(1, 0))
	if workerEligibleForWakeGeneration(existing, g, false) {
		t.Fatal("unverified worker eligible")
	}
	d.acknowledgeWorkerWake(existing, g)
	if !workerEligibleForWakeGeneration(existing, g, false) {
		t.Fatal("verified worker ineligible")
	}
	ready, total := d.wakeStatus(g)
	if ready != 1 || total != 1 {
		t.Fatalf("status %d/%d", ready, total)
	}
	replacement := wakeTestSlot()
	d.Register(replacement)
	select {
	case got := <-replacement.WakeCh:
		if got != g {
			t.Fatalf("generation %d", got)
		}
	default:
		t.Fatal("replacement not probed")
	}
}

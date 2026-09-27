package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

func acceptTestLifecycle(b *managedBridge, kind string) {
	acceptTestLifecycleRevision(b, kind, b.lifecycleSnapshot().Revision+1)
}

func acceptTestLifecycleRevision(b *managedBridge, kind string, revision uint64) {
	b.read(context.Background(), bufio.NewScanner(strings.NewReader(fmt.Sprintf(`{"v":1,"attempt_id":"wake-test","type":%q,"lifecycle_revision":%d}`+"\n", kind, revision))))
}

func TestManagedRuntimeEpochAndLifecycleRevisionFence(t *testing.T) {
	b := newManagedBridge(io.Discard, "wake-test", func() {})
	if first, second := b.nextRuntimeEpoch(), b.nextRuntimeEpoch(); first != 1 || second != 2 {
		t.Fatalf("runtime epochs %d/%d", first, second)
	}
	acceptTestLifecycleRevision(b, "device_wake", 7)
	if got := b.lifecycleSnapshot(); got.Revision != 7 || got.Sleeping {
		t.Fatalf("initial lifecycle = %+v", got)
	}
	acceptTestLifecycleRevision(b, "device_sleep", 7)
	acceptTestLifecycleRevision(b, "device_sleep", 6)
	if got := b.lifecycleSnapshot(); got.Revision != 7 || got.Sleeping {
		t.Fatalf("stale lifecycle mutated state: %+v", got)
	}
	acceptTestLifecycleRevision(b, "device_sleep", 8)
	if got := b.lifecycleSnapshot(); got.Revision != 8 || !got.Sleeping {
		t.Fatalf("fresh sleep missing: %+v", got)
	}
}

func TestManagedRuntimeEpochEchoValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  uint64
		ok    bool
	}{
		{name: "matching", value: float64(2), want: 2, ok: true},
		{name: "old runtime", value: float64(1), want: 2},
		{name: "missing", want: 2},
		{name: "string", value: "2", want: 2},
		{name: "fraction", value: 2.5, want: 2},
		{name: "zero", value: float64(0), want: 2},
		{name: "unsafe", value: float64(9007199254740992), want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := bridgeMessage{}
			if tc.value != nil {
				message["runtime_epoch"] = tc.value
			}
			got, valid := message.positiveSafeUint64("runtime_epoch")
			accepted := valid && got == tc.want
			if accepted != tc.ok {
				t.Fatalf("epoch=%v parsed=%d valid=%v accepted=%v", tc.value, got, valid, accepted)
			}
		})
	}
}

func TestManagedLifecycleSleepAfterJoinRejectsRestart(t *testing.T) {
	b := newManagedBridge(io.Discard, "wake-test", func() {})
	acceptTestLifecycle(b, "device_wake")
	old := b.lifecycleSnapshot()
	joined, resume := make(chan struct{}), make(chan struct{})
	result := make(chan bool, 1)
	go func() {
		close(joined)
		<-resume
		result <- b.admitLifecycleRestart(context.Background(), old.Revision, func() { t.Error("started after accepted sleep") })
	}()
	<-joined
	acceptTestLifecycle(b, "device_sleep")
	close(resume)
	if <-result {
		t.Fatal("stale restart admitted")
	}
	// Match the single runner owner's stopped/running guard on subsequent ticks.
	acceptTestLifecycle(b, "device_wake")
	current := b.lifecycleSnapshot()
	running, starts := false, 0
	for i := 0; i < 2; i++ {
		if !running {
			b.admitLifecycleRestart(context.Background(), current.Revision, func() { running = true; starts++ })
		}
	}
	if starts != 1 {
		t.Fatalf("later wake starts=%d", starts)
	}
}

func TestManagedLifecycleCancelAfterJoinRejectsRestart(t *testing.T) {
	b := newManagedBridge(io.Discard, "wake-test", func() {})
	acceptTestLifecycle(b, "device_wake")
	current := b.lifecycleSnapshot()
	ctx, cancel := context.WithCancel(context.Background())
	joined, resume := make(chan struct{}), make(chan struct{})
	result := make(chan bool, 1)
	go func() {
		close(joined)
		<-resume
		result <- b.admitLifecycleRestart(ctx, current.Revision, func() { t.Error("started after cancel") })
	}()
	<-joined
	cancel()
	close(resume)
	if <-result {
		t.Fatal("cancelled restart admitted")
	}
}

func TestManagedLifecycleRejectsWrongAttemptAndShape(t *testing.T) {
	b := newManagedBridge(io.Discard, "wake-test", func() {})
	for _, raw := range []string{
		`{"v":1,"attempt_id":"old","type":"device_wake"}`,
		`{"v":1,"attempt_id":"wake-test","type":"device_wake","extra":true}`,
		`{"v":1,"attempt_id":"wake-test","type":"device_wake"}`,
	} {
		b.read(context.Background(), bufio.NewScanner(strings.NewReader(raw+"\n")))
	}
	if b.lifecycleSnapshot().Revision != 0 {
		t.Fatal("invalid lifecycle accepted")
	}
	acceptTestLifecycle(b, "device_sleep")
	if s := b.lifecycleSnapshot(); s.Revision != 1 || !s.Sleeping {
		t.Fatal("valid sleep missing")
	}
}

func TestManagedLifecycleExplicitSleepRejectsLateLegacyWake(t *testing.T) {
	b := newManagedBridge(io.Discard, "wake-test", func() {})
	acceptTestLifecycleRevision(b, "device_sleep", 9)
	before := b.lifecycleSnapshot()
	if before.Revision != 9 || !before.Sleeping {
		t.Fatalf("explicit sleep missing: %+v", before)
	}
	b.read(context.Background(), bufio.NewScanner(strings.NewReader(
		`{"v":1,"attempt_id":"wake-test","type":"device_wake"}`+"\n")))
	after := b.lifecycleSnapshot()
	if after != before {
		t.Fatalf("legacy wake mutated explicit lifecycle: before=%+v after=%+v", before, after)
	}
	started := false
	if b.admitLifecycleRestart(context.Background(), after.Revision, func() { started = true }) || started {
		t.Fatal("legacy wake admitted worker restart after explicit sleep")
	}
}

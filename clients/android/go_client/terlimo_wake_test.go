package main

import (
	"testing"
	"time"
)

func TestManagedWakeRetainsHealthyAndPartial(t *testing.T) {
	for _, ready := range []int{1, 36} {
		var w managedWakeTracker
		now := time.Unix(100, 0)
		w.observe(1, false, now, func() {}, func(time.Time) uint64 { return 7 })
		if w.rescue(now.Add(15*time.Second), 7, ready) {
			t.Fatalf("restarted with %d recovered workers", ready)
		}
	}
}
func TestManagedWakeZeroRecoveryOnlyAfterGraceOnce(t *testing.T) {
	var w managedWakeTracker
	now := time.Unix(100, 0)
	w.observe(1, false, now, func() {}, func(time.Time) uint64 { return 9 })
	if w.rescue(now.Add(14*time.Second), 9, 0) || w.rescue(now.Add(15*time.Second), 8, 0) {
		t.Fatal("early or stale restart")
	}
	if !w.rescue(now.Add(15*time.Second), 9, 0) {
		t.Fatal("missing zero recovery rescue")
	}
	if w.rescue(now.Add(16*time.Second), 9, 0) {
		t.Fatal("duplicate rescue")
	}
}
func TestManagedWakeSleepAndNewGenerationInvalidate(t *testing.T) {
	var w managedWakeTracker
	now := time.Unix(100, 0)
	sleeps := 0
	generation := uint64(0)
	sleep := func() { sleeps++ }
	wake := func(time.Time) uint64 { generation++; return generation }
	w.observe(1, false, now, sleep, wake)
	w.observe(2, true, now.Add(time.Second), sleep, wake)
	if w.rescue(now.Add(time.Minute), 1, 0) {
		t.Fatal("restart after sleep")
	}
	w.observe(3, false, now.Add(2*time.Second), sleep, wake)
	if w.rescue(now.Add(time.Minute), 1, 0) {
		t.Fatal("old generation admitted")
	}
	if !w.rescue(now.Add(time.Minute), 2, 0) {
		t.Fatal("new generation not admitted")
	}
	w.observe(3, false, now.Add(time.Minute), sleep, wake)
	if sleeps != 1 || generation != 2 {
		t.Fatal("duplicate event repeated native work")
	}
}

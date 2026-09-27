package main

import (
	"testing"
)

func TestActiveWorkersTracksRegisteredChannelLifecycle(t *testing.T) {
	d := &Dispatcher{}
	empty := []*WorkerSlot{}
	d.workers.Store(&empty)
	if got := d.ActiveWorkers(); got != 0 {
		t.Fatalf("empty dispatcher active=%d", got)
	}
	first := wakeTestSlot()
	second := wakeTestSlot()
	d.Register(first)
	d.Register(second)
	if got := d.ActiveWorkers(); got != 2 {
		t.Fatalf("after two registrations active=%d", got)
	}
	d.Unregister(first)
	if got := d.ActiveWorkers(); got != 1 {
		t.Fatalf("after loss active=%d", got)
	}
	d.Unregister(second)
	if got := d.ActiveWorkers(); got != 0 {
		t.Fatalf("after last loss active=%d", got)
	}
}

func TestActiveWorkersLateOldSlotCannotChangeReplacementSet(t *testing.T) {
	d := &Dispatcher{}
	empty := []*WorkerSlot{}
	d.workers.Store(&empty)
	old := wakeTestSlot()
	d.Register(old)
	d.Unregister(old)
	replacement := wakeTestSlot()
	d.Register(replacement)
	if got := d.ActiveWorkers(); got != 1 {
		t.Fatalf("replacement active=%d", got)
	}
	d.Unregister(old)
	if got := d.ActiveWorkers(); got != 1 {
		t.Fatalf("late old unregister changed active=%d", got)
	}
}

func TestChannelsStatusMessageCarriesAttemptFacts(t *testing.T) {
	message := channelsStatusMessage(7, 3, 0, 34, 36)
	want := map[string]any{"type": "channels_status", "runtime_epoch": uint64(7), "lifecycle_revision": uint64(3),
		"generation": uint64(0), "active": 34, "target": 36}
	if len(message) != len(want) {
		t.Fatalf("keys=%v", message)
	}
	for key, value := range want {
		if message[key] != value {
			t.Fatalf("%s=%v want=%v", key, message[key], value)
		}
	}
}

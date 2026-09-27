package main

import "time"

// v18 wake rescue grace; independent of Connect and Switch budgets.
const managedWakeRescueDelay = 15 * time.Second

type managedWakeTracker struct {
	revision   uint64
	generation uint64
	due        time.Time
}

func (w *managedWakeTracker) observe(revision uint64, sleeping bool, now time.Time, sleep func(), wake func(time.Time) uint64) {
	if revision == w.revision {
		return
	}
	w.revision = revision
	// A new native wake generation replaces the old probe. Sending an
	// artificial sleep immediately before wake could reorder separate channels.
	w.generation = 0
	w.due = time.Time{}
	if sleeping {
		sleep()
	} else {
		w.generation = wake(now)
		w.due = now.Add(managedWakeRescueDelay)
	}
}

func (w *managedWakeTracker) rescue(now time.Time, generation uint64, ready int) bool {
	if w.generation == 0 || generation != w.generation || w.due.IsZero() || now.Before(w.due) {
		return false
	}
	w.due = time.Time{}
	// A verified channel in this generation proves a fresh path. Retain partial
	// availability; individual failed channels are already retried by WorkerGroup.
	return ready == 0
}

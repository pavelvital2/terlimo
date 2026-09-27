package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Production stdout is a nonblocking *os.File, so the real bridge writer honours write deadlines
// (bridgeDeadlineWriter). This fixture behaves the same way: once a deadline is set and has
// passed, Write fails, exactly like the production pipe after an exhausted switch budget.
type deadlineAwareWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	deadline time.Time
	writes   int
}

func (w *deadlineAwareWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = deadline
	return nil
}

func (w *deadlineAwareWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.deadline.IsZero() && !time.Now().Before(w.deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	w.writes++
	return w.buf.Write(p)
}

func (w *deadlineAwareWriter) contents() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *deadlineAwareWriter) writeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

// The exhausted switch budget must not be able to deliver the terminal failed switch_result:
// with a production-like deadline-aware writer the frame is refused deterministically,
// whichever ready branch the select picks (writeSlot claim or ctx.Done).
func TestExpiredBudgetCannotDeliverTerminalFrame(t *testing.T) {
	writer := &deadlineAwareWriter{}
	bridge := newManagedBridge(writer, "attempt-id", func() {})
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err := bridge.sendContext(expired, switchFailureMessage("node", "switch", "revision", "TRANSPORT_FAILED", true))
	if err == nil {
		t.Fatal("expired budget must not report a delivered terminal frame")
	}
	if writer.writeCount() != 0 || writer.contents() != "" {
		t.Fatal("expired budget must not write the terminal frame")
	}
}

// Deterministic cancellation path: with the write slot already held, an expired context has the
// only ready branch and the frame is refused without claiming the slot.
func TestBusyWriteSlotCancelsTerminalFrameDeterministically(t *testing.T) {
	writer := &deadlineAwareWriter{}
	bridge := newManagedBridge(writer, "attempt-id", func() {})
	bridge.writeSlot <- struct{}{}
	defer func() { <-bridge.writeSlot }()
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err := bridge.sendContext(expired, switchFailureMessage("node", "switch", "revision", "TRANSPORT_FAILED", true))
	if err == nil {
		t.Fatal("a busy write slot with an expired context must fail")
	}
	if writer.writeCount() != 0 || writer.contents() != "" {
		t.Fatal("cancelled terminal frame must not be written")
	}
}

// The production failed-frame helper used by the runner must deliver with the live sender and
// keep its correlation, code and rollback contract.
func TestLiveSenderDeliversFailedSwitchFrame(t *testing.T) {
	writer := &deadlineAwareWriter{}
	bridge := newManagedBridge(writer, "attempt-id", func() {})

	if err := bridge.send(switchFailureMessage("node", "switch", "revision", "TRANSPORT_FAILED", true)); err != nil {
		t.Fatalf("live terminal send failed: %v", err)
	}
	raw := writer.contents()
	for _, want := range []string{
		`"type":"switch_result"`, `"status":"failed"`, `"attempt_id":"attempt-id"`,
		`"code":"TRANSPORT_FAILED"`, `"rollback_allowed":true`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("terminal frame missing %s", want)
		}
	}
}

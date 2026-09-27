package main

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestAnnouncementBridgeActions proves the §11 host actions route to the right bounded
// channels with correlation, and that a malformed read fails closed with a bounded code
// instead of starting anything.
func TestAnnouncementBridgeActions(t *testing.T) {
	out := &bytes.Buffer{}
	bridge := newManagedBridge(out, "attempt-1", func() {})
	const hostKey = "host-key-0000000000000000000000000042"
	input := strings.Join([]string{
		`{"v":1,"attempt_id":"attempt-1","type":"announcements_list"}`,
		`{"v":1,"attempt_id":"attempt-1","type":"announcement_read","announcement_id":"an-1","idempotency_key":"` + hostKey + `"}`,
		`{"v":1,"attempt_id":"attempt-1","type":"announcement_read","announcement_id":"an-1","idempotency_key":"short"}`,
	}, "\n") + "\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge.read(ctx, bufio.NewScanner(strings.NewReader(input)))

	select {
	case <-bridge.announcements:
	default:
		t.Fatal("announcements_list was not routed to the bounded channel")
	}
	select {
	case read := <-bridge.announcementRead:
		if read.AnnouncementID != "an-1" || read.IdempotencyKey != hostKey {
			t.Fatalf("read request correlation lost: %+v", read)
		}
	default:
		t.Fatal("announcement_read was not routed to the bounded channel")
	}
	if !strings.Contains(out.String(), `"code":"INVALID_REQUEST"`) {
		t.Fatalf("malformed read did not fail closed: %s", out.String())
	}
}

func TestAnnouncementBridgeBoundaries(t *testing.T) {
	if !validAnnouncementBridgeID(strings.Repeat("a", 128)) {
		t.Fatal("128-char id must be accepted")
	}
	for _, bad := range []string{"", strings.Repeat("a", 129), "a\rb", "a\nb"} {
		if validAnnouncementBridgeID(bad) {
			t.Fatalf("id accepted: %q", bad)
		}
	}
	if !validAnnouncementBridgeKey("host-key-0000000000000000000000000042") {
		t.Fatal("16..128-char key must be accepted")
	}
	for _, bad := range []string{"", "short", strings.Repeat("k", 129), "key\r\ninject"} {
		if validAnnouncementBridgeKey(bad) {
			t.Fatalf("key accepted: %q", bad)
		}
	}
}

// TestManualRefreshBridgeAction proves the bounded refresh_manual host action routes to
// the runner wake channel (one cycle) and carries no payload.
func TestManualRefreshBridgeAction(t *testing.T) {
	out := &bytes.Buffer{}
	bridge := newManagedBridge(out, "attempt-1", func() {})
	input := `{"v":1,"attempt_id":"attempt-1","type":"refresh_manual"}` + "\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge.read(ctx, bufio.NewScanner(strings.NewReader(input)))
	select {
	case <-bridge.refreshManual:
	default:
		t.Fatal("refresh_manual was not routed to the runner wake channel")
	}
}

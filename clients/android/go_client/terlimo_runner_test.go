package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wg-turn-client/wlbs"
)

type runnerTestWriter func([]byte) (int, error)

func (f runnerTestWriter) Write(p []byte) (int, error) { return f(p) }

type observedPipeWriter struct {
	*os.File
	started chan struct{}
	once    sync.Once
	writes  atomic.Int32
}

func (w *observedPipeWriter) Write(p []byte) (int, error) {
	w.writes.Add(1)
	w.once.Do(func() { close(w.started) })
	return w.File.Write(p)
}

func runnerCatalog(now time.Time) *wlbs.Catalog {
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	return &wlbs.Catalog{V: 1, Status: "ok", SubscriptionRef: "sub", RegistrationID: "reg", SubscriptionStatus: "active", SubscriptionExpiresAt: stamp(time.Hour), SlotsLimit: 2, SlotsUsed: 1, Revision: "7", IssuedAt: stamp(0), RefreshAfter: stamp(5 * time.Minute), CatalogExpiresAt: stamp(15 * time.Minute), Nodes: []wlbs.Node{{Endpoint: wlbs.Endpoint{NodeID: "test", PeerIP: "192.0.2.1", DTLSPort: 443, DTLSSPKISHA256: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}, Name: "Test", CountryCode: "RU", WGPort: 51820, Protocol: "wdtt-v17", AuthMode: "installation-pop-v1", MaxWorkers: 36, Access: wlbs.Access{GrantID: "grant", DeviceID: "reg", Password: "private", VKHashes: []string{"hash"}, ExpiresAt: stamp(15 * time.Minute), Generation: "1", LeaseSeq: "1"}}}}
}

func TestManagedCatalogResponseSchema(t *testing.T) {
	cat := runnerCatalog(time.Now())
	for _, status := range []string{"ok", "complete"} {
		raw, _ := json.Marshal(map[string]any{"v": 1, "status": status, "catalog": cat})
		got, e := managedCatalogResponse(raw)
		if e != nil || got.Revision != "7" {
			t.Fatal("full wrapper rejected", e)
		}
	}
	for _, tc := range []struct{ raw, code string }{
		{`{"v":1,"status":"pending"}`, "OPERATION_PENDING"},
		{`{"v":1,"status":"unknown"}`, "OPERATION_UNKNOWN"},
		{`{"v":1,"status":"failed","code":"DEVICE_REVOKED"}`, "DEVICE_REVOKED"},
		{`{"v":1,"status":"failed"}`, "BAD_MESSAGE"},
		{`{"v":1,"status":"pending","catalog":{}}`, "BAD_MESSAGE"},
		{`{"v":1,"status":"unknown","catalog":{}}`, "BAD_MESSAGE"},
		{`{"v":1,"status":"complete"}`, "BAD_CATALOG"},
		{`{"v":1,"v":1,"status":"pending"}`, "BAD_MESSAGE"},
	} {
		_, e := managedCatalogResponse([]byte(tc.raw))
		if managedCode(e) != tc.code {
			t.Fatalf("got %v want %s", e, tc.code)
		}
	}
}

func TestManagedExpiredCatalogRetainsRevisionFence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerCatalog(now.Add(-time.Hour))
	store := &wlbs.CatalogStore{}
	if e := restoreManagedCatalog(store, old, "sub", now); e != nil {
		t.Fatal(e)
	}
	if store.Snapshot().Validate("sub", "reg", now) == nil {
		t.Fatal("expired snapshot became usable")
	}
	// S5-074: a same-revision re-issue may evolve only freshness/horizon literals;
	// it is accepted and the refreshed snapshot replaces the expired one.
	fresh := runnerCatalog(now)
	if e := store.Apply(fresh, "sub", "reg", now); e != nil {
		t.Fatalf("same-revision freshness refresh must be accepted: %v", e)
	}
	if got := store.Snapshot().CatalogExpiresAt; got != fresh.CatalogExpiresAt {
		t.Fatal("refreshed catalog horizon was not retained")
	}
	if e := store.Snapshot().Validate("sub", "reg", now); e != nil {
		t.Fatal("refreshed snapshot must be usable metadata", e)
	}
	// A material same-revision change still conflicts and never lands.
	material := runnerCatalog(now)
	material.Nodes[0].Access.Password = "other"
	if e := store.Apply(material, "sub", "reg", now); managedCode(e) != "REVISION_CONFLICT" {
		t.Fatal("same-revision material change accepted", e)
	}
	fresh.Revision = "6"
	if e := store.Apply(fresh, "sub", "reg", now); managedCode(e) != "STALE_CATALOG" {
		t.Fatal("revision rollback accepted", e)
	}
	old.Nodes[0].Access.DeviceID = "other"
	if restoreManagedCatalog(&wlbs.CatalogStore{}, old, "sub", now) == nil {
		t.Fatal("malformed expired cache accepted")
	}
}

func TestManagedNewRevisionCannotRollBackGrantFence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := runnerCatalog(now)
	old.Nodes[0].Access.Generation = "2"
	old.Nodes[0].Access.LeaseSeq = "3"
	store := &wlbs.CatalogStore{}
	if err := store.Apply(old, "sub", "reg", now); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"1", "9"}, {"2", "2"}} {
		next := runnerCatalog(now)
		next.Revision = "8"
		next.Nodes[0].Access.Generation = pair[0]
		next.Nodes[0].Access.LeaseSeq = pair[1]
		if err := store.Apply(next, "sub", "reg", now); managedCode(err) != "STALE_CATALOG" {
			t.Fatal("grant fence rollback accepted", err)
		}
	}
}

func TestManagedWGEndpointIsOnlyLocal(t *testing.T) {
	raw := "[Interface]\nPrivateKey = fake\n[Peer]\nPublicKey = fake\nEndpoint = 192.0.2.1:51820\n"
	got, e := managedWGConfig(raw, "12345")
	if e != nil || strings.Contains(got, "192.0.2.1") || !strings.Contains(got, "Endpoint = 127.0.0.1:12345") {
		t.Fatal("endpoint not normalized", e)
	}
	for _, port := range []string{"0", "-1", "65536", "12\nEndpoint = remote", "01"} {
		if _, e = managedWGConfig(raw, port); e == nil {
			t.Fatal("invalid port accepted")
		}
	}
	if _, e = managedWGConfig(raw+"Endpoint = 192.0.2.2:123\n", "12345"); e == nil {
		t.Fatal("multiple endpoints accepted")
	}
}

func TestManagedBridgeCancellationWinsLateReply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var b *managedBridge
	writes := 0
	b = newManagedBridge(runnerTestWriter(func(p []byte) (int, error) {
		writes++
		var m bridgeMessage
		if json.Unmarshal(p, &m) != nil {
			t.Fatal("bad request")
		}
		b.mu.Lock()
		ch := b.waiters[m.string("request_id")]
		b.mu.Unlock()
		cancel()
		ch <- bridgeMessage{"type": "persist_result"}
		return len(p), nil
	}), "attempt", cancel)
	if _, e := b.request(ctx, bridgeMessage{"type": "persist"}, "request_id", "persist_result"); !errors.Is(e, context.Canceled) {
		t.Fatal("late reply won cancellation", e)
	}
	if len(b.waiters) != 0 {
		t.Fatal("waiter leaked")
	}
	if _, e := b.request(ctx, bridgeMessage{"type": "persist"}, "request_id", "persist_result"); !errors.Is(e, context.Canceled) || writes != 1 {
		t.Fatal("cancelled request sent")
	}
}

func TestManagedBridgeShortWriteRejected(t *testing.T) {
	b := newManagedBridge(runnerTestWriter(func(p []byte) (int, error) { return len(p) - 1, nil }), "attempt", func() {})
	if e := b.send(bridgeMessage{"type": "test"}); !errors.Is(e, io.ErrShortWrite) {
		t.Fatal("partial bridge message accepted", e)
	}
}

func TestManagedBridgeProductionPipeWriteHonorsDeadline(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	b := newManagedBridge(w, "attempt", func() {})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = b.sendContext(ctx, bridgeMessage{"type": "test", "payload": strings.Repeat("x", 64*1024)})
	if err == nil || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("blocked production pipe ignored deadline: %v", err)
	}
}

func TestManagedBridgeInheritedStdoutSupportsDeadline(t *testing.T) {
	if os.Getenv("TERLIMO_INHERITED_STDOUT_CHILD") == "1" {
		out, err := managedBridgeOutput(os.Stdout)
		if err != nil {
			os.Exit(42)
		}
		defer out.Close()
		b := newManagedBridge(out, "attempt", func() {})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err = b.sendContext(ctx, bridgeMessage{"type": "persist"}); err != nil {
			os.Exit(42)
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestManagedBridgeInheritedStdoutSupportsDeadline$")
	cmd.Env = append(os.Environ(), "TERLIMO_INHERITED_STDOUT_CHILD=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, stdout); close(drained) }()
	err = cmd.Wait()
	<-drained
	if err != nil {
		t.Fatalf("inherited stdout cannot honor bridge deadline: %v", err)
	}
}

func TestManagedBridgeDeadlineWhileBackgroundWriterOwnsPipe(t *testing.T) {
	r, raw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w := &observedPipeWriter{File: raw, started: make(chan struct{})}
	b := newManagedBridge(w, "attempt", func() {})
	done := make(chan error, 1)
	go func() {
		done <- b.send(bridgeMessage{"type": "background", "payload": strings.Repeat("x", 64*1024)})
	}()
	<-w.started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err = b.sendContext(ctx, bridgeMessage{"type": "switch_result"})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || w.writes.Load() != 1 {
		t.Fatalf("switch write was late or not bounded: err=%v writes=%d", err, w.writes.Load())
	}
	_ = r.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background writer goroutine leaked after pipe close")
	}
	_ = raw.Close()
}

func TestManagedFailedPendingClearsOnlyAfterDurableACK(t *testing.T) {
	for _, ack := range []bool{false, true} {
		var b *managedBridge
		b = newManagedBridge(runnerTestWriter(func(p []byte) (int, error) {
			if !ack {
				return 0, io.ErrClosedPipe
			}
			var m bridgeMessage
			_ = json.Unmarshal(p, &m)
			b.mu.Lock()
			ch := b.waiters[m.string("request_id")]
			b.mu.Unlock()
			ch <- bridgeMessage{"type": "persist_result"}
			return len(p), nil
		}), "attempt", func() {})
		c := &managedController{bridge: b, saved: managedSaved{Pending: &wlbs.PendingOperation{RequestID: "original"}}}
		err := c.clearPending(context.Background())
		if ack && (err != nil || c.saved.Pending != nil) {
			t.Fatal("confirmed failure not cleared", err)
		}
		if !ack && (err == nil || c.saved.Pending == nil) {
			t.Fatal("pending lost before durable ACK")
		}
	}
}

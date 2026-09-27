package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// manualBoundaryServer wraps the wiring fixture and blocks each /me on a per-call
// release channel, so a runner cycle can be held in-flight deterministically without a
// sleep or a real network delay.
type manualBoundaryServer struct {
	fixture *wiringFixture
	mu      sync.Mutex
	meCount int
	hits    chan int
	release chan struct{}

	annEntered chan struct{}
	annRelease chan struct{}
	annOnce    sync.Once
}

func (s *manualBoundaryServer) handler() http.Handler {
	inner := s.fixture.handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/mobile/v1/me" {
			s.mu.Lock()
			s.meCount++
			n := s.meCount
			s.mu.Unlock()
			s.hits <- n
			<-s.release
		}
		if r.URL.Path == "/api/mobile/v1/announcements" && s.annEntered != nil {
			s.annOnce.Do(func() { close(s.annEntered) })
			<-s.annRelease
		}
		inner.ServeHTTP(w, r)
	})
}

func (s *manualBoundaryServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meCount
}

const manualRefreshLine = `{"v":1,"attempt_id":"attempt","type":"refresh_manual"}` + "\n"

// TestManualRefreshFullPathInFlightCoalesces drives the real bridge -> dispatcher ->
// Runner path (no direct Runner call): while the manual cycle is held in-flight at the
// server, two refresh_manual receipts must coalesce to that one cycle; a deliberate tap
// after completion starts exactly the next cycle.
func TestManualRefreshFullPathInFlightCoalesces(t *testing.T) {
	fixture := newWiringFixture(t)
	serverFixture := &manualBoundaryServer{fixture: fixture, hits: make(chan int, 16), release: make(chan struct{})}
	server := httptest.NewServer(serverFixture.handler())
	defer server.Close()

	var output bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge := newManagedBridge(&output, "attempt", cancel)
	start := managedStart{V: 1, Type: "start", AttemptID: "attempt",
		MobileBaseURL: server.URL, MobileEnvironment: "test"}
	signer := func(ctx context.Context, transcript []byte) ([]byte, error) {
		digest := sha256.Sum256(transcript)
		return ecdsa.SignASN1(rand.Reader, fixture.key, digest[:])
	}
	controller := &managedController{bridge: bridge, start: managedStart{MobileBaseURL: start.MobileBaseURL}}
	mobile, err := newManagedMobile(start, fixture.spkiDER, signer, bridge, controller)
	if err != nil {
		t.Fatal(err)
	}

	// A single live bridge reader (the production entry) so the refresh_manual receipts
	// really go through the bridge switch, not a direct channel send.
	reader, writer := io.Pipe()
	go bridge.read(ctx, bufio.NewScanner(reader))
	done := make(chan struct{})
	go func() {
		mobile.run(ctx, bridge)
		close(done)
	}()
	defer func() {
		cancel()
		_ = writer.Close()
		<-done
	}()
	sendManual := func() {
		if _, err := io.WriteString(writer, manualRefreshLine); err != nil {
			t.Fatalf("write refresh_manual: %v", err)
		}
	}

	waitHit := func(want int) {
		t.Helper()
		deadline := time.After(6 * time.Second)
		for {
			select {
			case n := <-serverFixture.hits:
				if n == want {
					return
				}
				// Unexpected earlier cycle: never hold it, release immediately.
				serverFixture.release <- struct{}{}
			case <-deadline:
				t.Fatalf("cycle /me #%d not observed (count=%d)", want, serverFixture.count())
			}
		}
	}

	// Initial periodic cycle (#1): release it.
	waitHit(1)
	serverFixture.release <- struct{}{}

	// Two rapid refresh_manual receipts on the real bridge.
	sendManual()
	sendManual()

	// Manual cycle (#2) starts and is held; the duplicate receipt must not add a cycle.
	waitHit(2)
	serverFixture.release <- struct{}{}

	// Stability window: the coalesced duplicate must not start a third cycle.
	select {
	case n := <-serverFixture.hits:
		t.Fatalf("duplicate manual receipt started an extra cycle: /me #%d", n)
	case <-time.After(400 * time.Millisecond):
	}

	// A deliberate tap after completion starts exactly one more cycle (#3).
	sendManual()
	waitHit(3)
	serverFixture.release <- struct{}{}

	if got := serverFixture.count(); got != 3 {
		t.Fatalf("cycles=%d want 3 (initial + one coalesced manual + one deliberate)", got)
	}
	if !strings.Contains(output.String(), `"type":"account_access"`) {
		t.Fatalf("no account_access reached the bridge: %q", output.String())
	}
}

// TestManualRefreshQueuedBehindBlockedDispatcher reproduces the exact ordering the
// correction asks for: manual cycle #2 is in-flight; the managedMobile dispatcher is
// blocked inside the synchronous announcements handler; a second refresh_manual is
// received by the bridge (buffered) meanwhile; then cycle #2 finishes and only then the
// dispatcher is released. A receipt that is still queued when the cycle finished must not
// start an extra manual cycle.
func TestManualRefreshQueuedBehindBlockedDispatcher(t *testing.T) {
	fixture := newWiringFixture(t)
	serverFixture := &manualBoundaryServer{
		fixture: fixture, hits: make(chan int, 16), release: make(chan struct{}),
		annEntered: make(chan struct{}), annRelease: make(chan struct{}),
	}
	server := httptest.NewServer(serverFixture.handler())
	defer server.Close()

	var output bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge := newManagedBridge(&output, "attempt", cancel)
	start := managedStart{V: 1, Type: "start", AttemptID: "attempt",
		MobileBaseURL: server.URL, MobileEnvironment: "test"}
	signer := func(ctx context.Context, transcript []byte) ([]byte, error) {
		digest := sha256.Sum256(transcript)
		return ecdsa.SignASN1(rand.Reader, fixture.key, digest[:])
	}
	controller := &managedController{bridge: bridge, start: managedStart{MobileBaseURL: start.MobileBaseURL}}
	mobile, err := newManagedMobile(start, fixture.spkiDER, signer, bridge, controller)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	go bridge.read(ctx, bufio.NewScanner(reader))
	done := make(chan struct{})
	go func() { mobile.run(ctx, bridge); close(done) }()
	defer func() { cancel(); _ = writer.Close(); <-done }()

	waitHit := func(want int) {
		t.Helper()
		deadline := time.After(6 * time.Second)
		for {
			select {
			case n := <-serverFixture.hits:
				if n == want {
					return
				}
				serverFixture.release <- struct{}{}
			case <-deadline:
				t.Fatalf("cycle /me #%d not observed (count=%d)", want, serverFixture.count())
			}
		}
	}
	send := func(line string) {
		if _, err := io.WriteString(writer, line+"\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Initial cycle #1.
	waitHit(1)
	serverFixture.release <- struct{}{}
	// Manual cycle #2 starts and is held.
	send(`{"v":1,"attempt_id":"attempt","type":"refresh_manual"}`)
	waitHit(2)

	// Block the dispatcher inside the synchronous announcements handler.
	send(`{"v":1,"attempt_id":"attempt","type":"announcements_list"}`)
	select {
	case <-serverFixture.annEntered:
	case <-time.After(4 * time.Second):
		t.Fatal("dispatcher never entered the announcements handler")
	}

	// Exact finish boundary: observe the manual cycle finish while the dispatcher is
	// still blocked, so the duplicate is provably received before finish₂.
	finished := make(chan struct{})
	var once sync.Once
	mobile.manualCycleFinishedCb = func() { once.Do(func() { close(finished) }) }

	// Second manual receipt while #2 is still held: prove the bridge received it.
	before := bridge.manualReceiptCount()
	send(`{"v":1,"attempt_id":"attempt","type":"refresh_manual"}`)
	received := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if bridge.manualReceiptCount() > before || len(bridge.refreshManual) == 1 {
			received = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !received {
		t.Fatalf("bridge never received the queued refresh_manual (receipts=%d len=%d)",
			bridge.manualReceiptCount(), len(bridge.refreshManual))
	}

	// Release cycle #2 and WAIT for its finish signal, still holding the dispatcher.
	serverFixture.release <- struct{}{}
	select {
	case <-finished:
	case <-time.After(4 * time.Second):
		t.Fatal("manual cycle #2 never finished")
	}
	// The duplicate must not have been delivered/acted on before finish₂.
	// Only now release the blocked dispatcher.
	serverFixture.annRelease <- struct{}{}

	// An extra manual cycle #3 must NOT appear before any explicit new tap.
	select {
	case n := <-serverFixture.hits:
		t.Fatalf("queued duplicate started an extra cycle: /me #%d", n)
	case <-time.After(600 * time.Millisecond):
	}

	// A deliberate tap after completion starts exactly the next cycle (#3).
	send(`{"v":1,"attempt_id":"attempt","type":"refresh_manual"}`)
	waitHit(3)
	serverFixture.release <- struct{}{}
	if got := serverFixture.count(); got != 3 {
		t.Fatalf("cycles=%d want 3 (initial + one in-flight manual + one deliberate)", got)
	}
}

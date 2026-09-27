package accountaccess

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// manualRunner counts completed runner cycles via a per-cycle stage hook and lets a test
// block any chosen cycle with channels (deterministic synchronizer, no sleep).
type manualRunner struct {
	*Runner
	mu     sync.Mutex
	cycles int
	hook   func(cycle int)
}

func (m *manualRunner) cycleCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cycles
}

func (m *manualRunner) manualRuns() int {
	m.Runner.mu.Lock()
	defer m.Runner.mu.Unlock()
	return m.Runner.manualRuns
}

func newManualRunner(t *testing.T, fixture *runnerFixture, server *httptest.Server) *manualRunner {
	t.Helper()
	session := newFixtureSession(t, fixture.auth, server)
	coordinator, err := NewCoordinator(Options{
		Client:    &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: session},
		AttemptID: "attempt-manual",
		Store:     &MemoryReceiptStore{},
		Subject:   session.Subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &manualRunner{}
	runner, err := NewRunner(RunnerConfig{
		Session:        session,
		Coordinator:    coordinator,
		Attempts:       3,
		RefreshFloor:   time.Minute,
		Jitter:         func(time.Duration) time.Duration { return 0 },
		Sleep:          func(context.Context, time.Duration) error { return nil },
		SelectedNodeID: func() string { return "" },
		OnStage: func(token string, _, _ int64) {
			if token != "AFTER_ME_READ" {
				return
			}
			m.mu.Lock()
			m.cycles++
			n := m.cycles
			hook := m.hook
			m.mu.Unlock()
			if hook != nil {
				hook(n)
			}
		},
		Emit: func(context.Context, map[string]any) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Runner = runner
	return m
}

func runManualRunner(t *testing.T, m *manualRunner) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = m.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

// (1) The shared wake channel is already occupied by a non-manual reason when the manual
// request arrives: the manual request must still run exactly one cycle, and a later tap
// must be accepted again.
func TestRunnerManualRefreshNotLostWhenWakeOccupied(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	m := newManualRunner(t, fixture, server)
	started1 := make(chan struct{})
	release1 := make(chan struct{})
	m.hook = func(cycle int) {
		if cycle == 1 {
			close(started1)
			<-release1
		}
	}
	stop := runManualRunner(t, m)
	defer stop()

	<-started1
	m.wake <- "foreign" // occupy the shared wake channel with another reason
	m.TriggerManual()   // must not be silently dropped
	close(release1)

	waitFor(t, func() bool { return m.manualRuns() >= 1 })
	time.Sleep(150 * time.Millisecond)
	if got := m.manualRuns(); got != 1 {
		t.Fatalf("manual request not coalesced to exactly one manual cycle: got %d", got)
	}
	m.TriggerManual() // a deliberate tap after completion starts the next cycle
	waitFor(t, func() bool { return m.manualRuns() >= 2 })
}

// (2) A duplicate tap while the already-started manual cycle is still running must not
// start a second manual cycle.
func TestRunnerManualRefreshDuplicateDuringManualCycle(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	m := newManualRunner(t, fixture, server)
	started1 := make(chan struct{})
	release1 := make(chan struct{})
	started2 := make(chan struct{})
	release2 := make(chan struct{})
	m.hook = func(cycle int) {
		switch cycle {
		case 1:
			close(started1)
			<-release1
		case 2:
			close(started2)
			<-release2
		}
	}
	stop := runManualRunner(t, m)
	defer stop()

	<-started1
	m.TriggerManual()
	close(release1)
	<-started2 // the manual cycle is now running (blocked)
	m.TriggerManual()
	close(release2)

	waitFor(t, func() bool { return m.manualRuns() >= 1 })
	time.Sleep(150 * time.Millisecond)
	if got := m.manualRuns(); got != 1 {
		t.Fatalf("duplicate tap started an extra manual cycle: got %d", got)
	}
	m.TriggerManual()
	waitFor(t, func() bool { return m.manualRuns() >= 2 })
}

// (3) A tap after completion starts the next cycle (also asserted at the end of (1)/(2)).
func TestRunnerManualRefreshAfterCompletionStartsNext(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	m := newManualRunner(t, fixture, server)
	stop := runManualRunner(t, m)
	defer stop()
	// A periodic cycle may run first; wait for the initial cycle, then drive two manual
	// cycles strictly one after another via cycle-count polling.
	waitFor(t, func() bool { return m.cycleCount() >= 1 })
	for target := 1; target <= 2; target++ {
		m.TriggerManual()
		deadline := time.Now().Add(2 * time.Second)
		for m.manualRuns() < target && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if got := m.manualRuns(); got < target {
			t.Fatalf("manual tap %d did not start a cycle: manualRuns=%d cycles=%d", target, got, m.cycleCount())
		}
	}
	if got := m.manualRuns(); got != 2 {
		t.Fatalf("exactly two deliberate taps must start two manual cycles: got %d", got)
	}
}

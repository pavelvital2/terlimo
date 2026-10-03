package accountaccess

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type idleSeedRunnerEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *idleSeedRunnerEvents) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}
func (e *idleSeedRunnerEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

func idleSeedRunnerWait(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func idleSeedRunnerStart(t *testing.T, runner *Runner) (context.Context, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = runner.Run(ctx) }()
	t.Cleanup(func() { cancel(); idleSeedRunnerWait(t, done, "runner stop") })
	return ctx, cancel, done
}

func TestIdleSeedRunnerPublicationAndFinishPrecedeHookForVerifiedAndBrowse(t *testing.T) {
	for _, browse := range []bool{false, true} {
		name := "verified"
		if browse {
			name = "browse"
		}
		t.Run(name, func(t *testing.T) {
			fixture, server := newRunnerFixture(t, false)
			if browse {
				fixture.gatewayBody = browseBody(3)
			}
			runner, _, _ := newRunner(t, fixture, server, nil, nil)
			events := &idleSeedRunnerEvents{}
			runner.config.OnVerified = func(MeResponse, CatalogResponse) { events.add("published-verified") }
			runner.config.OnBrowse = func(BrowseCatalogResponse) { events.add("published-browse") }
			runner.config.BeginAttempt = func(parent context.Context, _ bool) (context.Context, func()) {
				child, finish := context.WithCancel(parent)
				return child, func() { finish(); events.add("finish") }
			}
			hooked := make(chan struct{})
			var lifetime context.Context
			runner.config.IdleReady = func(ctx context.Context, nextCycle time.Time) bool {
				if ctx != lifetime || ctx.Err() != nil {
					t.Error("idle hook received finished/canceled attempt context")
				}
				if !nextCycle.After(time.Now()) {
					t.Error("optional boundary lost next-cycle deadline")
				}
				events.add("idle")
				close(hooked)
				return true
			}
			// Set the parent before starting Run to avoid a racing callback identity check.
			parent, cancel := context.WithCancel(context.Background())
			lifetime = parent
			done := make(chan struct{})
			go func() { defer close(done); _ = runner.Run(parent) }()
			t.Cleanup(func() { cancel(); idleSeedRunnerWait(t, done, "runner stop") })
			idleSeedRunnerWait(t, hooked, "post-publication idle hook")
			cancel()
			idleSeedRunnerWait(t, done, "runner stop")
			got := events.snapshot()
			wantedPublication := "published-verified"
			if browse {
				wantedPublication = "published-browse"
			}
			if len(got) != 3 || got[0] != wantedPublication || got[1] != "finish" || got[2] != "idle" {
				t.Fatalf("publication/finish/hook order: %v", got)
			}
		})
	}
}

func TestIdleSeedRunnerBusyCanRetryAtNextNaturalCycleThenNeverRepeats(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	runner, _, _ := newRunner(t, fixture, server, nil, nil)
	var calls, finishes atomic.Int32
	attempted := make(chan struct{}, 2)
	thirdFinished := make(chan struct{})
	runner.config.BeginAttempt = func(parent context.Context, _ bool) (context.Context, func()) {
		return parent, func() {
			if finishes.Add(1) == 3 {
				close(thirdFinished)
			}
		}
	}
	runner.config.IdleReady = func(context.Context, time.Time) bool {
		call := calls.Add(1)
		attempted <- struct{}{}
		return call != 1 // first boundary is busy; second consumes the one optional attempt
	}
	_, cancel, done := idleSeedRunnerStart(t, runner)
	idleSeedRunnerWait(t, attempted, "busy idle boundary")
	runner.Trigger("manual")
	idleSeedRunnerWait(t, attempted, "next natural successful boundary")
	runner.Trigger("wake")
	idleSeedRunnerWait(t, thirdFinished, "third foreground cycle")
	cancel()
	idleSeedRunnerWait(t, done, "runner stop")
	if calls.Load() != 2 {
		t.Fatalf("optional attempt repeated after consumption: %d hooks", calls.Load())
	}
}

func TestIdleSeedRunnerUnavailableOptionalResultDoesNotFailPrimaryCycle(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	runner, _, _ := newRunner(t, fixture, server, nil, nil)
	var hooks, publications, primaryErrors atomic.Int32
	firstHook := make(chan struct{})
	secondFinish := make(chan struct{})
	var finishes atomic.Int32
	runner.config.OnVerified = func(MeResponse, CatalogResponse) { publications.Add(1) }
	runner.config.OnError = func(string) { primaryErrors.Add(1) }
	runner.config.BeginAttempt = func(parent context.Context, _ bool) (context.Context, func()) {
		return parent, func() {
			if finishes.Add(1) == 2 {
				close(secondFinish)
			}
		}
	}
	runner.config.IdleReady = func(context.Context, time.Time) bool {
		// The optional owner handles this outcome locally and consumes it; Runner
		// receives no optional error and leaves the accepted catalogue usable.
		optionalError := errors.New("SERVICE_UNAVAILABLE")
		if optionalError == nil {
			t.Error("unavailable simulation unexpectedly succeeded")
		}
		hooks.Add(1)
		close(firstHook)
		return true
	}
	_, cancel, done := idleSeedRunnerStart(t, runner)
	idleSeedRunnerWait(t, firstHook, "consumed unavailable optional attempt")
	runner.TriggerManual()
	idleSeedRunnerWait(t, secondFinish, "foreground cycle after optional failure")
	cancel()
	idleSeedRunnerWait(t, done, "runner stop")
	if hooks.Load() != 1 || primaryErrors.Load() != 0 || publications.Load() != 2 {
		t.Fatalf("optional failure changed primary behavior: hooks=%d errors=%d publications=%d", hooks.Load(), primaryErrors.Load(), publications.Load())
	}
}

func TestIdleSeedRunnerForegroundTriggersPreemptBeforeQueueing(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	runner, _, _ := newRunner(t, fixture, server, nil, nil)
	preempts := 0
	runner.config.PreemptIdle = func() {
		preempts++
		if preempts == 1 && len(runner.wake) != 0 {
			t.Error("ordinary trigger queued before optional preemption")
		}
		if preempts == 2 && len(runner.manualWake) != 0 {
			t.Error("manual trigger queued before optional preemption")
		}
	}
	runner.Trigger("wake")
	runner.TriggerManual()
	runner.TriggerManual() // a coalesced foreground tap still fences optional work
	if preempts != 3 || len(runner.wake) != 1 || len(runner.manualWake) != 1 {
		t.Fatalf("foreground preemption/coalescing: preempts=%d wake=%d manual=%d", preempts, len(runner.wake), len(runner.manualWake))
	}
}

func TestIdleSeedRunnerForegroundTriggerReleasesOwnedHook(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	runner, _, _ := newRunner(t, fixture, server, nil, nil)
	hookEntered, preempted, nextFinished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var cancelOnce sync.Once
	runner.config.PreemptIdle = func() { cancelOnce.Do(func() { close(preempted) }) }
	runner.config.IdleReady = func(ctx context.Context, nextCycle time.Time) bool {
		close(hookEntered)
		select {
		case <-ctx.Done():
		case <-preempted:
		}
		return true
	}
	var finishes atomic.Int32
	runner.config.BeginAttempt = func(parent context.Context, _ bool) (context.Context, func()) {
		return parent, func() {
			if finishes.Add(1) == 2 {
				close(nextFinished)
			}
		}
	}
	_, cancel, done := idleSeedRunnerStart(t, runner)
	idleSeedRunnerWait(t, hookEntered, "owned optional hook")
	runner.TriggerManual()
	idleSeedRunnerWait(t, nextFinished, "foreground cycle after preemption")
	cancel()
	idleSeedRunnerWait(t, done, "runner stop")
}

func TestIdleSeedRunnerQueuedForegroundSkipsOptionalBoundary(t *testing.T) {
	fixture, server := newRunnerFixture(t, false)
	runner, _, _ := newRunner(t, fixture, server, nil, nil)
	var finishes, hooks atomic.Int32
	attempted := make(chan struct{})
	runner.config.BeginAttempt = func(parent context.Context, _ bool) (context.Context, func()) {
		return parent, func() {
			if finishes.Add(1) == 1 {
				runner.TriggerManual()
			}
		}
	}
	runner.config.IdleReady = func(context.Context, time.Time) bool {
		hooks.Add(1)
		if finishes.Load() != 2 {
			t.Errorf("optional work started before queued foreground: finishes=%d", finishes.Load())
		}
		close(attempted)
		return true
	}
	_, cancel, done := idleSeedRunnerStart(t, runner)
	idleSeedRunnerWait(t, attempted, "idle boundary after queued foreground")
	cancel()
	idleSeedRunnerWait(t, done, "runner stop")
	if hooks.Load() != 1 {
		t.Fatalf("optional boundary ran %d times", hooks.Load())
	}
}

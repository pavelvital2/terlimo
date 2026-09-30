package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// installCaptchaStub replaces the bridge emit/result pair for one test and answers every
// challenge after delay with the given value. Returns the emit counter.
func installCaptchaStub(t *testing.T, delay time.Duration, answer string) *atomic.Int32 {
	t.Helper()
	prevOutput, prevChan := managedCaptchaOutput, CaptchaResultChan
	var emits atomic.Int32
	chanCaptchaResult := make(chan CaptchaResult, 8)
	managedCaptchaOutput = func(id, mode, redirect, token string) {
		emits.Add(1)
		go func() {
			if delay > 0 {
				time.Sleep(delay)
			}
			chanCaptchaResult <- CaptchaResult{RequestID: id, Value: answer}
		}()
	}
	CaptchaResultChan = chanCaptchaResult
	t.Cleanup(func() {
		managedCaptchaOutput = prevOutput
		CaptchaResultChan = prevChan
	})
	return &emits
}

func captchaChallenge() *VkCaptchaError {
	return &VkCaptchaError{RedirectURI: "https://vk.com/captcha", SessionToken: "s"}
}

func TestCaptchaPauseHappensBeforeEmit(t *testing.T) {
	prevOutput, prevChan := managedCaptchaOutput, CaptchaResultChan
	defer func() {
		managedCaptchaOutput = prevOutput
		CaptchaResultChan = prevChan
	}()
	_, budget, stop := newOperationBudget(context.Background(), time.Second)
	defer stop()

	pausedAtEmit := -1
	CaptchaResultChan = make(chan CaptchaResult, 1)
	managedCaptchaOutput = func(id, mode, redirect, token string) {
		pausedAtEmit = budget.pausedNow()
		CaptchaResultChan <- CaptchaResult{RequestID: id, Value: "token"}
	}
	token, err := requestWebViewCaptcha(budgetCtxOf(t, budget), 0, captchaChallenge(), "auto", time.Second)
	if err != nil || token != "token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if pausedAtEmit != 1 {
		t.Fatalf("operation was paused %d times at emit, want 1 (pause must precede the bridge)", pausedAtEmit)
	}
}

// budgetCtxOf is a tiny helper: the budget context is the one returned next to the
// controller. Tests that keep the controller only re-derive it via the value.
func budgetCtxOf(t *testing.T, b *operationBudget) context.Context {
	t.Helper()
	return b.ctx
}

func TestOperationBudgetPauseSurvivesSlowBridgeAndKeepsRemaining(t *testing.T) {
	installCaptchaStub(t, 120*time.Millisecond, "slow-token")
	ctx, _, stop := newOperationBudget(context.Background(), 80*time.Millisecond)
	defer stop()

	start := time.Now()
	token, err := requestWebViewCaptcha(ctx, 0, captchaChallenge(), "manual", 5*time.Second)
	if err != nil || token != "slow-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Fatalf("wait did not exceed the operation budget: %v", elapsed)
	}
	// The remaining budget must still be usable and must not have pre-fired.
	select {
	case <-ctx.Done():
		t.Fatal("budget expired immediately after resume")
	case <-time.After(40 * time.Millisecond):
	}
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("expired budget keeps wrong semantics: %v", ctx.Err())
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("resumed budget never expired")
	}
}

func TestOperationBudgetAlreadyExpiredRefusesChallengeBeforeEmit(t *testing.T) {
	emits := installCaptchaStub(t, 0, "token")
	ctx, _, stop := newOperationBudget(context.Background(), 40*time.Millisecond)
	defer stop()
	time.Sleep(70 * time.Millisecond)

	_, err := requestWebViewCaptcha(ctx, 0, captchaChallenge(), "auto", time.Second)
	if err == nil {
		t.Fatal("expired operation must refuse the challenge")
	}
	if got := emits.Load(); got != 0 {
		t.Fatalf("expired operation emitted %d challenges, want 0", got)
	}
}

func TestOperationBudgetResumeCannotBeKilledByStaleTimerCallback(t *testing.T) {
	ctx, budget, stop := newOperationBudget(context.Background(), 90*time.Millisecond)
	defer stop()
	time.Sleep(30 * time.Millisecond)
	if paused, ok := budget.PauseForWait(); !paused || !ok {
		t.Fatalf("pause=(%v,%v), want the clock paused", paused, ok)
	}
	time.Sleep(130 * time.Millisecond) // far beyond the original deadline
	if ctx.Err() != nil {
		t.Fatalf("budget expired while paused: %v", ctx.Err())
	}
	budget.ResumeAfterWait()
	select {
	case <-ctx.Done():
		t.Fatal("a stale timer callback cancelled the resumed deadline")
	case <-time.After(35 * time.Millisecond):
	}
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("err=%v", ctx.Err())
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("resumed budget never expired")
	}
}

func TestInvalidatedLinkUsesDonorPathWithoutPausing(t *testing.T) {
	ctx, budget, stop := newOperationBudget(context.Background(), 100*time.Millisecond)
	defer stop()
	budget.Invalidate()
	time.Sleep(160 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("invalidated operation context must stay alive for the runtime: %v", ctx.Err())
	}
	emits := installCaptchaStub(t, 0, "donor-token")
	token, err := requestWebViewCaptcha(ctx, 0, captchaChallenge(), "auto", time.Second)
	if err != nil || token != "donor-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if emits.Load() != 1 {
		t.Fatalf("emits=%d, want 1", emits.Load())
	}
}

// TestPreparationWorkerCaptchaPausesOperationBudget is the production-shaped integration:
// the same helpers and worker/timer layout used by c.vpn / startManagedTunnel /
// cancelManagedPreparation, with a CAPTCHA wait longer than the operation budget.
func TestPreparationWorkerCaptchaPausesOperationBudget(t *testing.T) {
	installCaptchaStub(t, 140*time.Millisecond, "worker-token")
	parent, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()
	preparation, _, stop := newOperationBudget(parent, 80*time.Millisecond)
	defer stop()
	linked := linkOperationBudget(parent, preparation)    // same call as c.vpn
	workerCtx, cancelWorker := context.WithCancel(linked) // same shape as startManagedTunnel
	defer cancelWorker()
	stopCancel := context.AfterFunc(preparation, func() { cancelWorker() }) // same shape as cancelManagedPreparation
	defer stopCancel()

	token, err := requestWebViewCaptcha(workerCtx, 0, captchaChallenge(), "auto", 5*time.Second)
	if err != nil || token != "worker-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if preparation.Err() != nil {
		t.Fatalf("operation expired during the paused wait: %v", preparation.Err())
	}
	select {
	case <-workerCtx.Done():
		t.Fatal("worker was cancelled right after the wait instead of using the remaining budget")
	case <-time.After(40 * time.Millisecond):
	}
	select {
	case <-workerCtx.Done():
	case <-time.After(300 * time.Millisecond):
		t.Fatal("worker was not cancelled by the expired operation")
	}
	if !errors.Is(preparation.Err(), context.DeadlineExceeded) {
		t.Fatalf("operation Err=%v, want deadline semantics", preparation.Err())
	}
}

// TestOldInvalidatedOperationDoesNotPauseNewConnectBudget proves the correlation fence:
// a CAPTCHA of a finished/invalidated operation must not pause the new Connect budget.
func TestOldInvalidatedOperationDoesNotPauseNewConnectBudget(t *testing.T) {
	oldCtx, oldBudget, stopOld := newOperationBudget(context.Background(), 500*time.Millisecond)
	defer stopOld()
	oldBudget.Invalidate()
	oldWorker, cancelOld := context.WithCancel(linkOperationBudget(context.Background(), oldCtx))
	defer cancelOld()

	newCtx, _, stopNew := newOperationBudget(context.Background(), 120*time.Millisecond)
	defer stopNew()

	installCaptchaStub(t, 250*time.Millisecond, "old-token")
	done := make(chan error, 1)
	go func() {
		_, err := requestWebViewCaptcha(oldWorker, 0, captchaChallenge(), "manual", 5*time.Second)
		done <- err
	}()

	select {
	case <-newCtx.Done():
		if !errors.Is(newCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("new budget err=%v", newCtx.Err())
		}
	case <-time.After(400 * time.Millisecond):
		t.Fatal("new Connect budget was paused by an old invalidated operation")
	}
	if err := <-done; err != nil {
		t.Fatalf("old operation donor-path CAPTCHA failed: %v", err)
	}
}

// TestTrackedAttemptTimeoutPausesWithCaptchaWait covers the onboarding starter shape:
// its configured dial timeout keeps its duration and is only paused by the user wait.
func TestTrackedAttemptTimeoutPausesWithCaptchaWait(t *testing.T) {
	installCaptchaStub(t, 250*time.Millisecond, "onboarding-token")
	operation, budget, stop := newOperationBudget(context.Background(), 5*time.Second)
	defer stop()
	attemptCtx, cancelAttempt, ok := budget.TrackTimeout(100 * time.Millisecond)
	if !ok {
		t.Fatal("tracked timeout was not accepted")
	}
	defer cancelAttempt()

	token, err := requestWebViewCaptcha(attemptCtx, 0, captchaChallenge(), "manual", 5*time.Second)
	if err != nil || token != "onboarding-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if attemptCtx.Err() != nil {
		t.Fatalf("tracked dial timeout fired during the paused wait: %v", attemptCtx.Err())
	}
	select {
	case <-attemptCtx.Done():
		t.Fatal("tracked dial timeout fired immediately after resume")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-attemptCtx.Done():
		if !errors.Is(context.Cause(attemptCtx), context.DeadlineExceeded) {
			t.Fatalf("tracked timeout cause=%v", context.Cause(attemptCtx))
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("tracked dial timeout never fired after resume")
	}
	_ = operation
}

// ─── Root regression tests (applied with normal names) ───

func TestBaseDeadlineSurvivesTrackedTimeout(t *testing.T) {
	ctx, budget, stop := newOperationBudget(context.Background(), 30*time.Millisecond)
	defer stop()
	_, release, ok := budget.TrackTimeout(time.Second)
	if !ok {
		t.Fatal("tracked timeout was not accepted")
	}
	defer release()
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("base 30ms deadline disappeared after adding a 1s tracked timeout")
	}
}

func TestResumePreservesBaseDeadlineWithTrackedTimeout(t *testing.T) {
	ctx, budget, stop := newOperationBudget(context.Background(), 30*time.Millisecond)
	defer stop()
	_, release, _ := budget.TrackTimeout(time.Second)
	defer release()
	budget.PauseForWait()
	budget.ResumeAfterWait()
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("base deadline disappeared on resume with a tracked timeout")
	}
}

// ─── Timer lifecycle cases requested by root (behaviour, not implementation) ───

func TestTrackedTimerArmDoesNotCancelBaseOrSibling(t *testing.T) {
	baseCtx, budget, stop := newOperationBudget(context.Background(), 250*time.Millisecond)
	defer stop()
	first, releaseFirst, ok := budget.TrackTimeout(40 * time.Millisecond)
	if !ok {
		t.Fatal("first tracked timeout was not accepted")
	}
	defer releaseFirst()
	second, releaseSecond, ok := budget.TrackTimeout(90 * time.Millisecond)
	if !ok {
		t.Fatal("second tracked timeout was not accepted")
	}
	defer releaseSecond()

	select {
	case <-first.Done():
		if !errors.Is(context.Cause(first), context.DeadlineExceeded) {
			t.Fatalf("first child cause=%v", context.Cause(first))
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("first tracked timeout never fired")
	}
	if baseCtx.Err() != nil {
		t.Fatal("arming/firing a child cancelled the base budget")
	}
	select {
	case <-second.Done():
		if !errors.Is(context.Cause(second), context.DeadlineExceeded) {
			t.Fatalf("second child cause=%v", context.Cause(second))
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("sibling tracked timeout never fired")
	}
	if baseCtx.Err() != nil {
		t.Fatal("sibling firing cancelled the base budget")
	}
}

func TestReleasedTrackedTimerCallbackIsFenced(t *testing.T) {
	baseCtx, budget, stop := newOperationBudget(context.Background(), 300*time.Millisecond)
	defer stop()
	child, release, ok := budget.TrackTimeout(50 * time.Millisecond)
	if !ok {
		t.Fatal("tracked timeout was not accepted")
	}
	release()
	if !errors.Is(context.Cause(child), context.Canceled) {
		t.Fatalf("release must cancel the child with Canceled, cause=%v", context.Cause(child))
	}
	time.Sleep(90 * time.Millisecond)
	if baseCtx.Err() != nil {
		t.Fatal("base budget was cancelled by a released child's callback")
	}
	if !errors.Is(context.Cause(child), context.Canceled) {
		t.Fatalf("a stale released callback changed the child cause: %v", context.Cause(child))
	}
}

func TestPausedResumeFencesOldGenerationCallbacks(t *testing.T) {
	baseCtx, budget, stop := newOperationBudget(context.Background(), 80*time.Millisecond)
	defer stop()
	child, release, _ := budget.TrackTimeout(60 * time.Millisecond)
	defer release()
	time.Sleep(25 * time.Millisecond)
	if paused, usable := budget.PauseForWait(); !paused || !usable {
		t.Fatalf("pause=(%v,%v)", paused, usable)
	}
	time.Sleep(120 * time.Millisecond)
	if baseCtx.Err() != nil || child.Err() != nil {
		t.Fatal("paused clocks fired")
	}
	budget.ResumeAfterWait()
	select {
	case <-baseCtx.Done():
		t.Fatal("a stale base callback fired after resume")
	case <-child.Done():
		t.Fatal("a stale child callback fired after resume")
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case <-child.Done():
		if !errors.Is(context.Cause(child), context.DeadlineExceeded) {
			t.Fatalf("child cause=%v", context.Cause(child))
		}
	case <-time.After(120 * time.Millisecond):
		t.Fatal("child never expired after resume")
	}
	if baseCtx.Err() != nil {
		t.Fatal("base budget expired before its own remaining time")
	}
	select {
	case <-baseCtx.Done():
		if !errors.Is(baseCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("base err=%v", baseCtx.Err())
		}
	case <-time.After(120 * time.Millisecond):
		t.Fatal("base never expired after resume")
	}
}

func TestInvalidateStopsAllTimersWithoutCancellingContext(t *testing.T) {
	baseCtx, budget, stop := newOperationBudget(context.Background(), 60*time.Millisecond)
	defer stop()
	_, release, _ := budget.TrackTimeout(40 * time.Millisecond)
	defer release()
	budget.Invalidate()
	time.Sleep(120 * time.Millisecond)
	if baseCtx.Err() != nil {
		t.Fatal("invalidated budget context must stay alive for the live runtime")
	}
	if budget.expiredNow() {
		t.Fatal("invalidated budget must not expire in the background")
	}
}

func TestStopCancelsContextAndLateCallbacksDoNothing(t *testing.T) {
	baseCtx, budget, stop := newOperationBudget(context.Background(), 40*time.Millisecond)
	child, release, _ := budget.TrackTimeout(30 * time.Millisecond)
	defer release()
	stop()
	if !errors.Is(baseCtx.Err(), context.Canceled) {
		t.Fatalf("stop must cancel with Canceled, got %v", baseCtx.Err())
	}
	select {
	case <-child.Done():
		if errors.Is(context.Cause(child), context.DeadlineExceeded) {
			t.Fatal("a late child callback cancelled with DeadlineExceeded after stop")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("child of a stopped controller was not cancelled")
	}
	time.Sleep(60 * time.Millisecond)
	if !errors.Is(context.Cause(baseCtx), context.Canceled) {
		t.Fatalf("a late callback changed the stop cause: %v", context.Cause(baseCtx))
	}
}

func TestExpiredBudgetDoesNotReviveOrTrack(t *testing.T) {
	ctx, budget, stop := newOperationBudget(context.Background(), 30*time.Millisecond)
	defer stop()
	time.Sleep(60 * time.Millisecond)
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("err=%v", ctx.Err())
	}
	if _, _, ok := budget.TrackTimeout(time.Second); ok {
		t.Fatal("expired budget accepted a tracked timeout")
	}
	if paused, usable := budget.PauseForWait(); paused || usable {
		t.Fatalf("expired budget pause=(%v,%v)", paused, usable)
	}
	budget.ResumeAfterWait()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("expired budget revived")
	}
}

func TestBaseRemainsUpperBoundForLongTrackedTimeouts(t *testing.T) {
	baseCtx, budget, stop := newOperationBudget(context.Background(), 110*time.Millisecond)
	defer stop()
	shortCtx, releaseShort, _ := budget.TrackTimeout(30 * time.Millisecond)
	defer releaseShort()
	longCtx, releaseLong, _ := budget.TrackTimeout(600 * time.Millisecond)
	defer releaseLong()

	select {
	case <-shortCtx.Done():
	case <-time.After(120 * time.Millisecond):
		t.Fatal("short tracked timeout never fired")
	}
	if baseCtx.Err() != nil {
		t.Fatal("base expired before the short child fired")
	}
	select {
	case <-baseCtx.Done():
		if !errors.Is(baseCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("base err=%v", baseCtx.Err())
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("base budget outlived its deadline")
	}
	select {
	case <-longCtx.Done():
		if !errors.Is(context.Cause(longCtx), context.DeadlineExceeded) {
			t.Fatalf("long child cause=%v", context.Cause(longCtx))
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("long tracked timeout survived the base budget")
	}
}

func TestWaitContextIgnoresNarrowerDeadlineButFollowsDisconnect(t *testing.T) {
	parent, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	_, budget, stop := newOperationBudget(parent, 2*time.Second)
	defer stop()

	narrow, narrowCancel := context.WithTimeout(budget.ctx, 30*time.Millisecond)
	defer narrowCancel()
	waitCtx, releaseWait := budget.WaitContext(narrow, time.Second)
	defer releaseWait()
	time.Sleep(80 * time.Millisecond) // the narrower caller deadline already fired
	select {
	case <-waitCtx.Done():
		t.Fatalf("wait must ignore the caller's narrower deadline, cause=%v", context.Cause(waitCtx))
	case <-time.After(20 * time.Millisecond):
	}

	waitCtx2, releaseWait2 := budget.WaitContext(budget.ctx, time.Second)
	defer releaseWait2()
	disconnect()
	select {
	case <-waitCtx2.Done():
		if !errors.Is(context.Cause(waitCtx2), context.Canceled) {
			t.Fatalf("disconnect cause=%v", context.Cause(waitCtx2))
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("wait did not follow the real caller cancellation")
	}
}

func TestCaptchaWaitAbortsOnRealCancellationMidWait(t *testing.T) {
	prevOutput, prevChan := managedCaptchaOutput, CaptchaResultChan
	defer func() {
		managedCaptchaOutput = prevOutput
		CaptchaResultChan = prevChan
	}()
	release := make(chan struct{})
	CaptchaResultChan = make(chan CaptchaResult, 1)
	managedCaptchaOutput = func(id, mode, redirect, token string) {
		go func() {
			<-release
			CaptchaResultChan <- CaptchaResult{RequestID: id, Value: "late-token"}
		}()
	}

	parent, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	operation, _, stop := newOperationBudget(parent, 5*time.Second)
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := requestWebViewCaptcha(operation, 0, captchaChallenge(), "manual", 5*time.Second)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // the wait is in flight
	disconnect()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a real caller cancellation must abort the wait")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("wait ignored the real caller cancellation")
	}
	close(release)
}

// ─── Post-readiness lifetime (root regression + request-level checks) ───

// Root regression: after readiness the preparation is invalidated and canceled, but the
// live runtime keeps its handle; the post-readiness wait must follow the live runtime,
// never the completed preparation.
func TestPostReadinessCaptchaUsesLiveRuntime(t *testing.T) {
	runtime, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	prep, budget, stop := newOperationBudget(runtime, time.Second)
	worker := linkOperationBudget(runtime, prep)
	budget.Invalidate()
	stop()
	if worker.Err() != nil {
		t.Fatal("worker unexpectedly stopped")
	}
	paused, usable := budget.PauseForWait()
	if paused || !usable {
		t.Fatal("post-readiness should use the donor wait")
	}
	wait, release := budget.WaitContext(worker, time.Second)
	defer release()
	select {
	case <-wait.Done():
		t.Fatalf("live runtime CAPTCHA aborted by completed preparation: %v", context.Cause(wait))
	case <-time.After(20 * time.Millisecond):
	}
}

// The real request path on a live runtime: challenge emitted, answer delivered, token
// returned — even though the preparation was invalidated and canceled before the call.
func TestPostReadinessCaptchaSolvesThroughRequestPath(t *testing.T) {
	installCaptchaStub(t, 40*time.Millisecond, "post-ready-token")
	runtime, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	prep, budget, stop := newOperationBudget(runtime, time.Second)
	worker := linkOperationBudget(runtime, prep)
	budget.Invalidate()
	stop()

	token, err := requestWebViewCaptcha(worker, 0, captchaChallenge(), "manual", time.Second)
	if err != nil || token != "post-ready-token" {
		t.Fatalf("post-readiness token=%q err=%v", token, err)
	}
}

// Readiness/wait creation race: the preparation is canceled after the wait has been
// created. The wait must still survive on the live runtime.
func TestPostReadinessWaitSurvivesPreparationCancelAfterCreation(t *testing.T) {
	prevOutput, prevChan := managedCaptchaOutput, CaptchaResultChan
	defer func() {
		managedCaptchaOutput = prevOutput
		CaptchaResultChan = prevChan
	}()
	answer := make(chan string, 1)
	emitted := make(chan string, 1)
	CaptchaResultChan = make(chan CaptchaResult, 1)
	managedCaptchaOutput = func(id, mode, redirect, token string) {
		emitted <- id
		go func() {
			value := <-answer
			CaptchaResultChan <- CaptchaResult{RequestID: id, Value: value}
		}()
	}

	runtime, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	prep, budget, stop := newOperationBudget(runtime, time.Second)
	worker := linkOperationBudget(runtime, prep)
	budget.Invalidate()

	done := make(chan struct {
		token string
		err   error
	}, 1)
	go func() {
		token, err := requestWebViewCaptcha(worker, 0, captchaChallenge(), "manual", time.Second)
		done <- struct {
			token string
			err   error
		}{token, err}
	}()
	<-emitted
	stop() // preparation canceled while the bounded wait is already in flight
	time.Sleep(30 * time.Millisecond)
	select {
	case result := <-done:
		t.Fatalf("wait died with the preparation: token=%q err=%v", result.token, result.err)
	default:
	}
	answer <- "race-token"
	select {
	case result := <-done:
		if result.err != nil || result.token != "race-token" {
			t.Fatalf("token=%q err=%v", result.token, result.err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("post-readiness wait never completed")
	}
}

// A real Disconnect of the live runtime still aborts the post-readiness wait.
func TestPostReadinessWaitFollowsDisconnect(t *testing.T) {
	prevOutput, prevChan := managedCaptchaOutput, CaptchaResultChan
	defer func() {
		managedCaptchaOutput = prevOutput
		CaptchaResultChan = prevChan
	}()
	release := make(chan struct{})
	emitted := make(chan string, 1)
	CaptchaResultChan = make(chan CaptchaResult, 1)
	managedCaptchaOutput = func(id, mode, redirect, token string) {
		emitted <- id
		go func() {
			<-release
			CaptchaResultChan <- CaptchaResult{RequestID: id, Value: "late-token"}
		}()
	}

	runtime, disconnect := context.WithCancel(context.Background())
	prep, budget, stop := newOperationBudget(runtime, time.Second)
	worker := linkOperationBudget(runtime, prep)
	budget.Invalidate()
	stop()
	_ = prep

	done := make(chan error, 1)
	go func() {
		_, err := requestWebViewCaptcha(worker, 0, captchaChallenge(), "auto", time.Second)
		done <- err
	}()
	<-emitted
	disconnect()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Disconnect must abort the post-readiness wait")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("post-readiness wait ignored Disconnect")
	}
	close(release)
}

package accountaccess

// Runner is the single runtime owner of retry/refresh for the embedded mobile session.
// The coordinator coalesces concurrent refreshes; the runner only decides when to ask.
// Triggers (wake, network recovery, operation terminal, proven deadline) never create a
// second coordinator or a second business authority.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// RunnerConfig wires the runner to the session/coordinator pair and to safe callbacks.
// Callbacks must not receive credentials and must not re-enter the coordinator.
type RunnerConfig struct {
	Session     *MobileSession
	Coordinator *Coordinator
	Emit        func(ctx context.Context, payload map[string]any) error
	OnVerified  func(me MeResponse, catalog CatalogResponse)
	// DisplayOnly chooses the explicit browse read for this operation. It never
	// starts/replays sync and never publishes a credential pair.
	DisplayOnly func(context.Context) bool
	// OnMe receives the accepted current subject snapshot after a successful refresh,
	// including a duplicate. Canceled/stale operations cannot update this cache.
	OnMe func(MeResponse)
	// OnBrowse receives the display-only browse catalog. It never replaces the
	// verified pair, never feeds the store/admission path and is never emitted
	// through OnVerified.
	OnBrowse    func(browse BrowseCatalogResponse)
	OnAdmission func(AdmissionDecision)
	OnError     func(code string)
	// SelectedNodeID returns the current native selection; empty skips the admission
	// callback because there is nothing to admit yet.
	SelectedNodeID func() string
	// OnStage receives one fixed cycle-stage token with the monotonic elapsed since the
	// attempt start and the current UTC ms. Optional, secret-free and observation-only:
	// it never changes the cycle order or any decision.
	OnStage func(token string, elapsedMS, utcMS int64)
	// OnManualCycleFinished is an optional observation hook fired immediately after
	// the manual-refresh fence is cleared (manual cycle finished). Nil is a no-op and it
	// never re-enters the coordinator.
	OnManualCycleFinished func()
	// BeginAttempt binds one complete retry group to its host operation. The bool
	// identifies TriggerManual (a host catalog operation), not an ordinary
	// Trigger("manual") used to refresh credentials for explicit Connect. finish
	// runs after the manual fence is cleared, so a replacement operation can wake
	// this same runner without being lost behind the canceled request.
	BeginAttempt func(context.Context, bool) (context.Context, func())
	// IdleReady runs on this owner after successful catalogue publication and finish.
	// false means busy/unpublished; retry only at a later natural successful cycle.
	// true consumes the single optional attempt, even on cancellation or unavailable data.
	// The existing next-cycle deadline bounds optional work; it cannot defer renewal.
	IdleReady    func(context.Context, time.Time) bool
	PreemptIdle  func()
	Attempts     int
	Jitter       func(time.Duration) time.Duration
	Sleep        func(ctx context.Context, delay time.Duration) error
	RefreshFloor time.Duration
	Now          func() time.Time
}

// Fixed cycle-stage vocabulary of one mobile attempt. The host mirror accepts exactly
// this set (plus the gateway status-class tokens mapped by GatewayRequestStage); the
// runner never emits anything else. Values are the token and integers only.
const (
	cycleStageAfterMeRead       = "AFTER_ME_READ"
	cycleStageEmitBegin         = "EMIT_BEGIN"
	cycleStageEmitEndOK         = "EMIT_END_OK"
	cycleStageEmitEndErr        = "EMIT_END_ERR"
	cycleStageEmitEndCancel     = "EMIT_END_CANCEL"
	cycleStageGWRefreshBegin    = "GW_REFRESH_BEGIN"
	cycleStageGWPendingRefresh  = "GW_PENDING_REFRESH_BEGIN"
	cycleStageGWPendingEnd      = "GW_PENDING_REFRESH_END"
	cycleStageAttemptRetrySleep = "ATTEMPT_RETRY_SLEEP"
	cycleStageAttemptRetryWait  = "ATTEMPT_RETRY_WAIT"
	cycleStageAttemptTerminal   = "ATTEMPT_TERMINAL"
)

// Runner performs bounded cycles and waits on the next refresh point or a trigger.
type Runner struct {
	config    RunnerConfig
	wake      chan string
	stageBase time.Time
	// renewedFor is the proven deadline horizon that already had its single forced
	// renewal. It is owned by the single Run loop (focused tests drive cycle directly
	// on one goroutine): a pending or failed renewal never mints a second key for the
	// same horizon, while a new (extended or shortened) horizon may renew again.
	renewedFor time.Time

	// Manual-refresh coalescing, decoupled from the shared wake channel. manualQueued is
	// the in-flight fence: it is set on the first accepted manual request and cleared only
	// when the cycle that request caused completes, so repeats inside that cycle are
	// dropped. manualWake is a dedicated, buffered signal so a manual request can never be
	// lost when the shared wake channel is already occupied by another reason.
	mu           sync.Mutex
	manualQueued bool
	manualRuns   int
	manualWake   chan struct{}
}

// renewLead is the bounded early-renewal lead for the on-demand mobile node lease. The
// backend renews a grant only for an authenticated POST /access/sync (ensure_grant), and
// only while the remaining lease is under max_lease/3 (= 300 s); backend maintenance does
// not renew, and an idempotent replay of an applied receipt creates no new effect. 240 s
// starts the renewal at most 60 s before the backend threshold, leaving the whole
// remaining lease as bounded slack for the POST, the poll and the catalog refresh before
// the proven deadline stops the data plane. The lead is a constant, not a new TTL.
const renewLead = 240 * time.Second

// NewRunner validates the configuration.
func NewRunner(config RunnerConfig) (*Runner, error) {
	if config.Session == nil || config.Coordinator == nil {
		return nil, fmt.Errorf("accountaccess runner requires session and coordinator")
	}
	if config.Emit == nil {
		return nil, fmt.Errorf("accountaccess runner requires an emitter")
	}
	if config.Attempts <= 0 {
		config.Attempts = 3
	}
	if config.RefreshFloor <= 0 {
		config.RefreshFloor = time.Minute
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if config.Sleep == nil {
		config.Sleep = sleepContext
	}
	// The attempt starts when the single runner is constructed: one monotonic base for
	// every cycle-stage marker of this attempt (wall-clock steps cannot distort it).
	return &Runner{config: config, wake: make(chan string, 1), manualWake: make(chan struct{}, 1), stageBase: time.Now()}, nil
}

// stage emits one fixed cycle-stage marker with the monotonic elapsed since the attempt
// start and the current UTC ms. A missing callback stays silent; the elapsed value is
// bounded to the fixed numeric field of the host contract.
func (r *Runner) stage(token string) {
	if r.config.OnStage == nil {
		return
	}
	elapsed := time.Since(r.stageBase).Milliseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > stageElapsedCapMS {
		elapsed = stageElapsedCapMS
	}
	r.config.OnStage(token, elapsed, time.Now().UnixMilli())
}

// Trigger requests an immediate refresh. Triggers coalesce: at most one is pending.
func (r *Runner) Trigger(reason string) {
	if r.config.PreemptIdle != nil {
		r.config.PreemptIdle()
	}
	select {
	case r.wake <- reason:
	default:
	}
}

// TriggerManual requests exactly one manual refresh cycle. Rapid repeats from the first
// accepted request until that cycle completes coalesce into the same cycle; a deliberate
// request after completion starts the next cycle. The pending signal travels on a
// dedicated buffered channel, so a manual request is never lost when the shared wake
// channel already holds another reason. Other wake reasons are unaffected.
func (r *Runner) TriggerManual() {
	if r.config.PreemptIdle != nil {
		r.config.PreemptIdle()
	}
	r.mu.Lock()
	if r.manualQueued {
		r.mu.Unlock()
		// Fixed, secret-free diagnostic: this receipt was coalesced by the in-flight fence.
		fmt.Fprintln(os.Stderr, "refreshstage:coalesced")
		return
	}
	r.manualQueued = true
	r.mu.Unlock()
	fmt.Fprintln(os.Stderr, "refreshstage:accepted")
	// Coalesced: a signal already waiting means the owed manual cycle has not started yet.
	select {
	case r.manualWake <- struct{}{}:
	default:
	}
}

func (r *Runner) finishManualRun() {
	r.mu.Lock()
	r.manualQueued = false
	r.mu.Unlock()
}

// Run owns the refresh loop until the context is cancelled. A non-retryable API result
// (for example an explicit revoke) is reported through OnAdmission and does not spin.
func (r *Runner) Run(ctx context.Context) error {
	manual := false
	optionalDone := false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// An already queued explicit refresh wins over a simultaneously ready
		// periodic/retry timer; do not spend its host budget on a background cycle.
		if !manual {
			select {
			case <-r.manualWake:
				manual = true
			default:
			}
		}
		if manual {
			r.mu.Lock()
			r.manualRuns++
			r.mu.Unlock()
			fmt.Fprintln(os.Stderr, "refreshstage:manual_cycle_begin")
		}
		attemptCtx := ctx
		finish := func() {}
		if r.config.BeginAttempt != nil {
			attemptCtx, finish = r.config.BeginAttempt(ctx, manual)
		}
		catalog, browse, wait, err := r.attempt(attemptCtx)
		if manual {
			manual = false
			r.finishManualRun()
			fmt.Fprintln(os.Stderr, "refreshstage:manual_cycle_finish")
			if r.config.OnManualCycleFinished != nil {
				r.config.OnManualCycleFinished()
			}
		}
		finish()
		if err != nil {
			if r.config.OnError != nil {
				r.config.OnError(safeErrorCode(err))
			}
		} else if catalog != nil {
			wait = time.Until(r.nextCycleAt(*catalog))
		} else if browse != nil {
			wait = time.Until(BrowseRefreshAt(*browse, r.config.Now(), r.config.RefreshFloor))
		} else {
			// Neither a verified pair nor a display list is available yet (for example a
			// nonterminal admission pending): keep the floor cadence, never a hot loop.
			wait = r.config.RefreshFloor
		}
		// Publication/finish happened above; queued foreground refresh always wins.
		if !optionalDone && wait > 0 && err == nil && (catalog != nil || browse != nil) &&
			r.config.IdleReady != nil && len(r.wake) == 0 && len(r.manualWake) == 0 {
			idleStart := time.Now()
			optionalDone = r.config.IdleReady(ctx, idleStart.Add(wait))
			wait -= time.Since(idleStart)
		}
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		case <-r.manualWake:
			timer.Stop()
			manual = true
		case <-r.wake:
			timer.Stop()
			// Ordinary credential refresh stays untagged, even when its
			// historical reason string is "manual". Only manualWake binds
			// the next iteration to a host catalog cycle.
		}
	}
}

// AuthenticateServiceOnce is the explicit recovery readiness boundary. It uses the
// same session/client, but does not fetch a catalogue, sync a grant, persist account
// receipts or start the recurring Run loop. A signed endpoint is not enough: a
// current installation session and its strictly decoded /me must both succeed.
func (r *Runner) AuthenticateServiceOnce(ctx context.Context) error {
	if err := r.config.Session.Ensure(ctx); err != nil {
		return err
	}
	subject, generation := r.config.Session.Subject(), r.config.Session.Generation()
	me, apiError, err := r.config.Coordinator.opts.Client.GetMe(ctx)
	if err != nil {
		return err
	}
	if apiError != nil {
		return errors.New("RECOVERY_NETWORK")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	account := ""
	if me.AccountRef != nil {
		account = *me.AccountRef
	}
	if generation == "" || generation != r.config.Session.Generation() || subject != r.config.Session.Subject() || account != subject.AccountRef {
		return errors.New("RECOVERY_NETWORK")
	}
	return nil
}

// attempt runs one bounded cycle and returns the verified catalog or the display-only
// browse catalog (when reached) and the delay before the next attempt.
func (r *Runner) attempt(ctx context.Context) (*CatalogResponse, *BrowseCatalogResponse, time.Duration, error) {
	var lastErr error
	for attempt := 1; attempt <= r.config.Attempts; attempt++ {
		catalog, browse, err := r.cycle(ctx)
		if err == nil {
			return catalog, browse, 0, nil
		}
		lastErr = err
		if !runnerRetryable(err) {
			// A terminal API result (revoke, missing entitlement) must not hot-loop:
			// wait for the healthy refresh cadence or an explicit wake trigger.
			r.stage(cycleStageAttemptTerminal)
			return nil, nil, r.config.RefreshFloor, lastErr
		}
		if attempt == r.config.Attempts {
			break
		}
		var status *APIStatusError
		var retryAfter *int
		if errors.As(err, &status) {
			retryAfter = status.RetryAfterMS
		}
		r.stage(cycleStageAttemptRetrySleep)
		if sleepErr := r.config.Sleep(ctx, RetryDelay(attempt, retryAfter, r.config.Jitter)); sleepErr != nil {
			return nil, nil, 0, sleepErr
		}
	}
	wait := RetryDelay(1, nil, r.config.Jitter)
	r.stage(cycleStageAttemptRetryWait)
	return nil, nil, wait, lastErr
}

func (r *Runner) cycle(ctx context.Context) (*CatalogResponse, *BrowseCatalogResponse, error) {
	if err := r.config.Session.Ensure(ctx); err != nil {
		return nil, nil, err
	}
	result, err := r.config.Coordinator.Refresh(ctx)
	r.stage(cycleStageAfterMeRead)
	if err != nil {
		var status *APIStatusError
		if errors.As(err, &status) && (status.Code == "SESSION_EXPIRED" || status.Code == "SESSION_INVALID") {
			// Same subject/session chain: baseline and uncertain receipts are preserved.
			r.config.Session.Refresh()
			r.config.Coordinator.NewSession()
		}
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	displayOnly := r.config.DisplayOnly != nil && r.config.DisplayOnly(ctx)
	if result.State == StateApplied && !result.Dropped && !result.Coalesced {
		if me, ok := currentMe(r.config.Coordinator); ok && r.config.OnMe != nil {
			r.config.OnMe(me)
		}
	} else if displayOnly {
		// An unaccepted /me cannot authorize publication for this display cycle.
		return nil, nil, nil
	}
	if result.Projection != nil {
		r.stage(cycleStageEmitBegin)
		if err := r.config.Emit(ctx, result.Projection.Payload()); err != nil {
			if ctx.Err() != nil {
				r.stage(cycleStageEmitEndCancel)
			} else {
				r.stage(cycleStageEmitEndErr)
			}
			return nil, nil, err
		}
		r.stage(cycleStageEmitEndOK)
	}
	r.stage(cycleStageGWRefreshBegin)
	if displayOnly {
		return r.displayCatalog(ctx)
	}
	catalogResult, err := r.config.Coordinator.RefreshCatalog(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if catalogResult.Browse != nil {
		// Display-only branch: no admission decision, no store projection and no
		// access/sync. The host receives the metadata list through its own callback.
		if r.config.OnBrowse != nil {
			r.config.OnBrowse(*catalogResult.Browse)
		}
		return nil, catalogResult.Browse, nil
	}
	catalog := catalogResult.Catalog
	// A pending catalog (ACCESS_SYNC_PENDING with admission tokens, or 503) is
	// nonterminal: after the writer runs, the required me/catalog refresh is done in
	// THIS cycle so the verified pair/admission fire without waiting for the healthy
	// floor cadence. A still-pending result returns a retryable error (bounded
	// Attempts/RetryDelay), never a hot loop and never a new timeout budget.
	pendingCatalog := catalog == nil && catalogResult.Admission != nil
	me := lastMe(r.config.Coordinator)
	if catalog != nil && r.config.OnVerified != nil {
		// The verified pair is handed to the transport owner before admission: it may
		// project the snapshot, but it never re-fetches or re-decides business rights.
		r.config.OnVerified(*me, *catalog)
	}
	admitted := false
	selected := ""
	if catalog != nil && r.config.SelectedNodeID != nil {
		selected = r.config.SelectedNodeID()
		if selected != "" {
			now := r.config.Now()
			decision := DecideAdmission(*me, *catalog, selected, now)
			admitted = decision.Admitted
			if r.config.OnAdmission != nil {
				r.config.OnAdmission(decision)
			}
		}
	}
	if !containsString(r.config.Session.Scopes(), "access:sync") {
		return catalog, nil, nil
	}
	plan := renewalPlan{}
	if catalog != nil {
		plan = r.renewalPlan(*me, *catalog, selected, admitted, r.config.Now())
	}
	if plan.due {
		// On-demand lease renewal: the backend renews only for an authenticated
		// access.sync POST, so an unchanged-digest replay cannot refresh the lease.
		// Exactly one NEW durable key (same admissible body/revisions) is minted for
		// this horizon and persisted before the POST. The window is recorded before the
		// send, so a failed or lost response resumes the SAME key instead of minting
		// another; the response then goes through the existing pending/poll/refresh
		// machinery below, never through a parallel verifier.
		r.renewedFor = plan.deadline
		if _, err := r.config.Coordinator.SyncAccessForced(ctx); err != nil {
			return catalog, nil, err
		}
		plan.outstanding = true
	}
	sync, err := r.config.Coordinator.SyncAccess(ctx)
	if err != nil {
		return catalog, nil, err
	}
	if sync.Receipt != nil && sync.Receipt.Response != nil &&
		sync.Receipt.Response.AccessApplicationState == "pending" {
		polled, err := r.config.Coordinator.PollAccessOperation(ctx)
		if err != nil {
			return catalog, nil, err
		}
		// A pending receipt can become recoverable during this poll. Hand the
		// durable server marker back to the existing coordinator now rather than
		// spending another ME/catalog cycle (and a possible transport rotation).
		// SyncAccess retains subject/revision/key fences and owns the one rotation;
		// an operation result never substitutes for a verified catalog below.
		if !polled.Stale && polled.Receipt != nil && polled.Receipt.RecoveryRequired {
			if _, err := r.config.Coordinator.SyncAccess(ctx); err != nil {
				return catalog, nil, err
			}
		}
	}
	if pendingCatalog || plan.outstanding {
		r.stage(cycleStageGWPendingRefresh)
		refreshed, err := r.config.Coordinator.RefreshCatalog(ctx)
		r.stage(cycleStageGWPendingEnd)
		if err != nil {
			return catalog, nil, err
		}
		if refreshed.Browse != nil {
			if r.config.OnBrowse != nil {
				r.config.OnBrowse(*refreshed.Browse)
			}
			return catalog, refreshed.Browse, nil
		}
		if refreshed.Catalog == nil {
			return catalog, nil, errors.New("ACCESS_SYNC_PENDING")
		}
		catalog = refreshed.Catalog
		if r.config.OnVerified != nil {
			r.config.OnVerified(*lastMe(r.config.Coordinator), *catalog)
		}
		if r.config.OnAdmission != nil && r.config.SelectedNodeID != nil {
			if selected := r.config.SelectedNodeID(); selected != "" {
				r.config.OnAdmission(DecideAdmission(*lastMe(r.config.Coordinator), *catalog, selected, r.config.Now()))
			}
		}
	}
	return catalog, nil, nil
}

func (r *Runner) displayCatalog(ctx context.Context) (*CatalogResponse, *BrowseCatalogResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// A list refresh must still enforce fresh revocation for an already selected
	// VPN. Use only the existing verified catalog, never the metadata response.
	var retained *CatalogResponse
	if me, ok := currentMe(r.config.Coordinator); ok && r.config.SelectedNodeID != nil {
		if selected := r.config.SelectedNodeID(); selected != "" {
			retained = r.config.Coordinator.LastGood()
			if retained != nil && r.config.OnAdmission != nil {
				r.config.OnAdmission(DecideAdmission(me, *retained, selected, r.config.Now()))
			}
		}
	}
	result, err := r.config.Coordinator.RefreshDisplayCatalog(ctx)
	if err != nil {
		return retained, nil, err
	}
	if err := ctx.Err(); err != nil {
		return retained, nil, err
	}
	if result.Browse != nil && r.config.OnBrowse != nil {
		r.config.OnBrowse(*result.Browse)
	}
	// Retain the selected VPN's refresh/lease schedule instead of replacing it
	// with the unrelated browse cache TTL.
	return retained, result.Browse, nil
}

func currentMe(coordinator *Coordinator) (MeResponse, bool) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.lastMe == nil || !coordinator.subjectSet ||
		coordinator.lastMeSubject != coordinator.subject || coordinator.lastMeTick != coordinator.sessionTick ||
		coordinator.subject != coordinator.opts.Subject() {
		return MeResponse{}, false
	}
	return *coordinator.lastMe, true
}

// lastMe is a read-only snapshot helper; it never mutates the coordinator.
func lastMe(coordinator *Coordinator) *MeResponse {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.lastMe == nil {
		return &MeResponse{}
	}
	copy := *coordinator.lastMe
	return &copy
}

// renewalDeadline is the finite proven stop bound of the admitted right: the same
// minimum the mobile proven-deadline timer uses. The entitlement effective_deadline is
// skipped only when null (allowed for the indefinite commercial right), while the
// catalog validity and the selected node lease always participate.
func renewalDeadline(me MeResponse, catalog CatalogResponse, selectedNodeID string) time.Time {
	var earliest time.Time
	consider := func(raw string) {
		parsed, err := parseUtc(raw)
		if err != nil {
			return
		}
		if earliest.IsZero() || parsed.Before(earliest) {
			earliest = parsed
		}
	}
	if me.GrantResolution.EffectiveDeadline != nil {
		consider(*me.GrantResolution.EffectiveDeadline)
	}
	consider(catalog.ValidUntil)
	for _, gateway := range catalog.Gateways {
		if gateway.GatewayID != selectedNodeID {
			continue
		}
		consider(gateway.Access.NotAfter)
		break
	}
	return earliest
}

// renewalPlan is the on-demand lease renewal decision for one verified, admitted pair.
// due starts the single forced renewal of this deadline; outstanding means the deadline
// already had its forced renewal and the refreshed horizon must be observed before the
// proven deadline, at the bounded floor cadence. A revoked/expired right is never
// admitted by DecideAdmission and therefore never reaches either state.
type renewalPlan struct {
	deadline    time.Time
	due         bool
	outstanding bool
}

func (r *Runner) renewalPlan(me MeResponse, catalog CatalogResponse, selected string, admitted bool, now time.Time) renewalPlan {
	if !admitted || selected == "" {
		return renewalPlan{}
	}
	deadline := renewalDeadline(me, catalog, selected)
	if deadline.IsZero() || !deadline.After(now) {
		return renewalPlan{}
	}
	if r.renewedFor.Equal(deadline) {
		return renewalPlan{deadline: deadline, outstanding: true}
	}
	return renewalPlan{deadline: deadline, due: !deadline.After(now.Add(renewLead))}
}

// nextCycleAt is the single-owner wait after a verified catalog: the healthy catalog
// refresh point, shortened to the early-renewal point (proven deadline - renewLead) so
// the on-demand renewal starts on time, and to the bounded floor cadence while an
// attempted renewal is still unresolved. A shortened fresh horizon shortens the renewal
// point exactly as it shortens the deadline; no max() is applied anywhere.
func (r *Runner) nextCycleAt(catalog CatalogResponse) time.Time {
	now := r.config.Now()
	next := NextRefreshAt(catalog, now, r.config.RefreshFloor)
	if r.config.SelectedNodeID == nil || !containsString(r.config.Session.Scopes(), "access:sync") {
		return next
	}
	selected := r.config.SelectedNodeID()
	if selected == "" {
		return next
	}
	me := lastMe(r.config.Coordinator)
	plan := r.renewalPlan(*me, catalog, selected, DecideAdmission(*me, catalog, selected, now).Admitted, now)
	if plan.deadline.IsZero() {
		return next
	}
	wake := plan.deadline.Add(-renewLead)
	if plan.outstanding {
		wake = now.Add(r.config.RefreshFloor)
	} else if wake.Before(now) {
		wake = now
	}
	if wake.Before(next) {
		next = wake
	}
	return next
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func runnerRetryable(err error) bool {
	var status *APIStatusError
	if errors.As(err, &status) {
		return status.Retryable
	}
	var popErr *PopError
	if errors.As(err, &popErr) {
		return false
	}
	return true
}

func safeErrorCode(err error) string {
	var status *APIStatusError
	if errors.As(err, &status) && status.Code != "" {
		return status.Code
	}
	var popErr *PopError
	if errors.As(err, &popErr) && popErr.Code != "" {
		return popErr.Code
	}
	return "TRANSPORT"
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

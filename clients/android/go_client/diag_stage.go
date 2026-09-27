package main

// Fixed, secret-free per-attempt chronology markers of one S5 mobile attempt. The host
// mirror (NativeStderrMirror.kt) accepts exactly this vocabulary and writes the fixed
// formats to logcat; anything else is dropped. Only a fixed token, one bounded index
// and bounded integers (monotonic per-attempt elapsed ms, UTC ms and a token-specific
// detail ms) ever travel here: never a hash, credential, token, URL, status text, body
// or IP.
//
// The markers exist to distinguish, on a real run, where one attempt stalls:
//   - the /me wire response boundary (ME_RESPONSE_*) versus the post-decode
//     Coordinator.Refresh return (AFTER_ME_READ): the gap is the decode/post-processing,
//   - pipe-write/emit stall after /me (AFTER_ME_READ -> EMIT_BEGIN -> EMIT_END_*),
//   - the pause between the refresh request and the catalog refresh (GW_REFRESH_BEGIN),
//   - the actual GET /gateways request and its HTTP status class
//     (GW_REQUEST_BEGIN -> GW_REQUEST_END_*),
//   - bounded cold-VK substages (CACHE_*, *_WAIT, FETCH_*, PROVIDER_*, HASH_*).

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	cycleStageAfterMeRead      = "AFTER_ME_READ"
	cycleStageEmitBegin        = "EMIT_BEGIN"
	cycleStageEmitEndOK        = "EMIT_END_OK"
	cycleStageEmitEndErr       = "EMIT_END_ERR"
	cycleStageEmitEndCancel    = "EMIT_END_CANCEL"
	cycleStageGWRefreshBegin   = "GW_REFRESH_BEGIN"
	cycleStageGWPendingRefresh = "GW_PENDING_REFRESH_BEGIN"
	cycleStageGWRequestBegin   = "GW_REQUEST_BEGIN"
)

const (
	vkStageCacheHit       = "CACHE_HIT"
	vkStageCacheMiss      = "CACHE_MISS"
	vkStageSerialLockWait = "SERIAL_LOCK_WAIT"
	vkStageThrottleWait   = "THROTTLE_WAIT"
	vkStageFetchBegin     = "FETCH_BEGIN"
	vkStageFetchEndOK     = "FETCH_END_OK"
	vkStageFetchEndErr    = "FETCH_END_ERR"
	vkStageProviderModern = "PROVIDER_MODERN"
	vkStageProviderLegacy = "PROVIDER_LEGACY"
	vkStageHashBegin      = "HASH_BEGIN"
	vkStageHashEnd        = "HASH_END"
)

// diagCycleTokens is the fixed cyclestage vocabulary; no other value may reach stderr.
var diagCycleTokens = map[string]bool{
	"ME_RESPONSE_2XX":          true,
	"ME_RESPONSE_4XX":          true,
	"ME_RESPONSE_5XX":          true,
	"ME_RESPONSE_TRANSPORT":    true,
	"ME_RESPONSE_OTHER":        true,
	cycleStageAfterMeRead:      true,
	cycleStageEmitBegin:        true,
	cycleStageEmitEndOK:        true,
	cycleStageEmitEndErr:       true,
	cycleStageEmitEndCancel:    true,
	cycleStageGWRefreshBegin:   true,
	cycleStageGWPendingRefresh: true,
	cycleStageGWRequestBegin:   true,
	"GW_REQUEST_END_2XX":       true,
	"GW_REQUEST_END_4XX":       true,
	"GW_REQUEST_END_5XX":       true,
	"GW_REQUEST_END_TRANSPORT": true,
	"GW_REQUEST_END_OTHER":     true,
}

// diagVKTokens is the fixed vkstage vocabulary; no other value may reach stderr.
var diagVKTokens = map[string]bool{
	vkStageCacheHit:       true,
	vkStageCacheMiss:      true,
	vkStageSerialLockWait: true,
	vkStageThrottleWait:   true,
	vkStageFetchBegin:     true,
	vkStageFetchEndOK:     true,
	vkStageFetchEndErr:    true,
	vkStageProviderModern: true,
	vkStageProviderLegacy: true,
	vkStageHashBegin:      true,
	vkStageHashEnd:        true,
}

// diagStageDiagnostics is the stderr sink of the fixed stage markers. Production keeps
// os.Stderr; focused tests substitute a buffer without changing behavior.
var diagStageDiagnostics io.Writer = os.Stderr

const (
	diagElapsedMaxMS = 9_999_999
	diagUTCMinMS     = 1_000_000_000_000
	diagUTCMaxMS     = 9_999_999_999_999
	diagIndexMax     = 999
	diagDetailMaxMS  = 9_999_999
)

// emitCycleStage writes exactly one `cyclestage: TOKEN <elapsed_ms> <utc_ms>` line for a
// fixed token and in-range integers; anything else produces no output.
func emitCycleStage(token string, elapsedMS, utcMS int64) {
	if !diagCycleTokens[token] || elapsedMS < 0 || elapsedMS > diagElapsedMaxMS ||
		utcMS < diagUTCMinMS || utcMS > diagUTCMaxMS {
		return
	}
	fmt.Fprintf(diagStageDiagnostics, "cyclestage: %s %d %d\n", token, elapsedMS, utcMS)
}

// vpnstage CANCEL_SOURCE vocabulary: the FIRST cause that cancelled THIS VPN attempt's
// preparation context. Recorded on the per-attempt managedDiagnostics (first-writer-wins),
// emitted once at the terminal VPN_SETUP_FAILED; no endpoint, key, token or exception.
const (
	vpnCancelHostStop       = "HOST_STOP"
	vpnCancelConnectBudget  = "CONNECT_BUDGET"
	vpnCancelSwitch         = "SWITCH"
	vpnCancelRevoked        = "REVOKED"
	vpnCancelParentShutdown = "PARENT_SHUTDOWN"
	vpnCancelWorkerGate     = "WORKER_GATE"
	vpnCancelWorkerCreds    = "WORKER_CREDS"
	vpnCancelWorkerTerminal = "WORKER_TERMINAL"
	vpnCancelUnknown        = "UNKNOWN"
)

// The three worker-group cancel sites have distinct secret-free tokens so a run can tell
// the startup gate from the initial-credential failure from a managed terminal error.
// WORKER_ERROR stays absent (a specific credential-failure label is not proven).
var diagVPNSources = map[string]bool{
	vpnCancelHostStop: true, vpnCancelConnectBudget: true,
	vpnCancelSwitch: true, vpnCancelRevoked: true, vpnCancelParentShutdown: true,
	vpnCancelWorkerGate: true, vpnCancelWorkerCreds: true, vpnCancelWorkerTerminal: true,
	vpnCancelUnknown: true,
}

// workerCancelDiagKey carries the attempt-local diagnostics into the worker context so each
// group.go cancel site can record its own reason before cancelling.
type workerCancelDiagKey struct{}

func withWorkerCancelDiagnostics(ctx context.Context, d *managedDiagnostics) context.Context {
	return context.WithValue(ctx, workerCancelDiagKey{}, d)
}

func noteWorkerCancelSite(ctx context.Context, token string) {
	if d, ok := ctx.Value(workerCancelDiagKey{}).(*managedDiagnostics); ok && d != nil {
		d.noteCancelSource(token)
	}
}

// emitVPNSource writes exactly one sanitized `vpnstage: CANCEL_SOURCE <TOKEN>` line; any
// value outside the fixed vocabulary collapses to UNKNOWN.
func emitVPNSource(token string) {
	if !diagVPNSources[token] {
		token = vpnCancelUnknown
	}
	fmt.Fprintf(diagStageDiagnostics, "vpnstage: CANCEL_SOURCE %s\n", token)
}

// emitRuntimeCancel is the producer-side bounded origin of a live connected runtime
// cancel (the downstream consumer only sees TUNNEL_STOPPED). Same fixed vocabulary as
// CANCEL_SOURCE; anything else collapses to UNKNOWN.
func emitRuntimeCancel(token string) {
	if !diagVPNSources[token] {
		token = vpnCancelUnknown
	}
	fmt.Fprintf(diagStageDiagnostics, "vpnstage: RUNTIME_CANCEL %s\n", token)
}

// usagestage is the bounded HTTP response class of the authenticated /usage read.
var diagUsageStages = map[string]bool{
	"RESPONSE_2XX": true, "RESPONSE_4XX": true, "RESPONSE_5XX": true, "RESPONSE_OTHER": true,
	// Bounded first-failure cause of the /usage read (separate from the public usage_result
	// code, which stays TRANSPORT/MOBILE_STATE_UNAVAILABLE for the frozen host contract).
	"FAIL_LOCAL_SEED_MISSING": true, "FAIL_LOCAL_ORIGIN_REJECTED": true,
	"FAIL_LOCAL_PATH_REJECTED": true, "FAIL_LOCAL_REQUEST_REJECTED": true,
	"FAIL_SERVICE_RESPONSE_REJECTED": true, "FAIL_SERVICE_PATH_DENIED": true,
	"FAIL_SERVICE_UNAVAILABLE": true, "FAIL_SERVICE_ERROR": true,
	"FAIL_TRANSPORT_TIMEOUT": true, "FAIL_TRANSPORT_FAILED": true,
	"FAIL_TRUST_FAILED": true, "FAIL_VK_API_UNAVAILABLE": true, "FAIL_TRANSPORT": true,
}

func usageResponseStage(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "RESPONSE_2XX"
	case status >= 400 && status < 500:
		return "RESPONSE_4XX"
	case status >= 500 && status < 600:
		return "RESPONSE_5XX"
	default:
		return "RESPONSE_OTHER"
	}
}

// emitUsageStage writes one bounded `usagestage: TOKEN` line; unknown tokens produce none.
func emitUsageStage(token string) {
	if !diagUsageStages[token] {
		return
	}
	fmt.Fprintf(diagStageDiagnostics, "usagestage: %s\n", token)
}

// emitVKStage writes exactly one
// `vkstage: TOKEN <index> <elapsed_ms> <utc_ms> <detail_ms>` line for a fixed token and
// in-range integers; anything else produces no output. elapsedMS is the per-attempt
// elapsed (comparable across every vkstage line); detailMS is the token-specific bounded
// duration (the measured wait for the *_WAIT tokens, 0 elsewhere).
func emitVKStage(token string, index int, elapsedMS, utcMS, detailMS int64) {
	if !diagVKTokens[token] || index < 0 || index > diagIndexMax ||
		elapsedMS < 0 || elapsedMS > diagElapsedMaxMS ||
		utcMS < diagUTCMinMS || utcMS > diagUTCMaxMS ||
		detailMS < 0 || detailMS > diagDetailMaxMS {
		return
	}
	fmt.Fprintf(diagStageDiagnostics, "vkstage: %s %d %d %d %d\n", token, index, elapsedMS, utcMS, detailMS)
}

// diagAttempt marks the start of one mobile attempt. The time.Time keeps its monotonic
// component so elapsed values cannot jump on a wall-clock step. It is set once at the
// mobile attempt start before the attempt goroutines spawn; the mutex only keeps the
// read safe for any late goroutine.
var diagAttempt = struct {
	mu   sync.Mutex
	base time.Time
}{}

// diagMarkAttemptStart is called once at the actual mobile attempt start.
func diagMarkAttemptStart() {
	diagAttempt.mu.Lock()
	diagAttempt.base = time.Now()
	diagAttempt.mu.Unlock()
}

// diagElapsedMS is the monotonic elapsed since the attempt start, clamped to the fixed
// numeric field so a long-lived process still emits valid lines. No attempt start means 0.
func diagElapsedMS() int64 {
	diagAttempt.mu.Lock()
	base := diagAttempt.base
	diagAttempt.mu.Unlock()
	if base.IsZero() {
		return 0
	}
	elapsed := time.Since(base).Milliseconds()
	if elapsed < 0 {
		return 0
	}
	if elapsed > diagElapsedMaxMS {
		return diagElapsedMaxMS
	}
	return elapsed
}

// diagEmitCycle emits one fixed cycle-stage marker anchored to the attempt start.
func diagEmitCycle(token string) {
	emitCycleStage(token, diagElapsedMS(), time.Now().UnixMilli())
}

// diagEmitVK emits one fixed VK-stage marker anchored to the attempt start, with a zero
// detail field (no token-specific duration for this event).
func diagEmitVK(token string, index int) {
	emitVKStage(token, index, diagElapsedMS(), time.Now().UnixMilli(), 0)
}

// diagEmitVKWait emits one fixed VK wait marker: elapsed keeps the per-attempt elapsed
// (the same comparable base as every vkstage line) and the measured wait travels in the
// separate detail field, so wait durations remain unambiguous.
func diagEmitVKWait(token string, index int, waitMS int64) {
	emitVKStage(token, index, diagElapsedMS(), time.Now().UnixMilli(), waitMS)
}

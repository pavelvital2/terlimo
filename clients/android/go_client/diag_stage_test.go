package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	cycleStageLine = regexp.MustCompile(`^cyclestage: ([A-Z0-9_]{1,64}) ([0-9]{1,7}) ([0-9]{13})$`)
	vkStageLine    = regexp.MustCompile(`^vkstage: ([A-Z0-9_]{1,64}) ([0-9]{1,3}) ([0-9]{1,7}) ([0-9]{13}) ([0-9]{1,7})$`)
)

// The two emitters must write exactly the accepted grammar; the host mirror accepts
// only these two forms. This is the emitter half of the delivery contract.
func TestDiagStageEmittersExactOutput(t *testing.T) {
	var buf strings.Builder
	previous := diagStageDiagnostics
	diagStageDiagnostics = &buf
	defer func() { diagStageDiagnostics = previous }()

	emitCycleStage(cycleStageAfterMeRead, 123, 1790356109000)
	emitCycleStage(cycleStageGWPendingRefresh, 9999999, 9999999999999)
	emitVKStage(vkStageCacheHit, 0, 5, 1790356109000, 0)
	emitVKStage(vkStageHashEnd, 999, 7, 1000000000000, 2500)

	want := "cyclestage: AFTER_ME_READ 123 1790356109000\n" +
		"cyclestage: GW_PENDING_REFRESH_BEGIN 9999999 9999999999999\n" +
		"vkstage: CACHE_HIT 0 5 1790356109000 0\n" +
		"vkstage: HASH_END 999 7 1000000000000 2500\n"
	if buf.String() != want {
		t.Fatalf("emitter output:\n got %q\nwant %q", buf.String(), want)
	}
}

// Every fixed token must produce a line of the exact grammar with only the token and
// the bounded integers: no free text, hash, credential or URL can pass.
func TestDiagStageEmitterVocabularyGrammar(t *testing.T) {
	var buf strings.Builder
	previous := diagStageDiagnostics
	diagStageDiagnostics = &buf
	defer func() { diagStageDiagnostics = previous }()

	for token := range diagCycleTokens {
		emitCycleStage(token, 1, 1790356109000)
	}
	for token := range diagVKTokens {
		emitVKStage(token, 1, 2, 1790356109000, 3)
	}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if len(line) > 96 {
			t.Fatalf("line exceeds the mirror cap: %q", line)
		}
		if cycleStageLine.MatchString(line) || vkStageLine.MatchString(line) {
			continue
		}
		t.Fatalf("line outside the fixed grammar: %q", line)
	}
	if got := strings.Count(buf.String(), "\n"); got != len(diagCycleTokens)+len(diagVKTokens) {
		t.Fatalf("emitted %d lines, want %d", got, len(diagCycleTokens)+len(diagVKTokens))
	}
}

// Unknown tokens, out-of-range integers and any extra content produce no output at all:
// a hostile value must never reach the host mirror.
func TestDiagStageEmittersRejectInvalidInputSilently(t *testing.T) {
	var buf strings.Builder
	previous := diagStageDiagnostics
	diagStageDiagnostics = &buf
	defer func() { diagStageDiagnostics = previous }()

	emitCycleStage("NOT_A_STAGE", 1, 1790356109000)
	emitCycleStage("", 1, 1790356109000)
	emitCycleStage("AFTER_ME_READ token=sekrit", 1, 1790356109000)
	emitCycleStage("after_me_read", 1, 1790356109000)
	emitCycleStage(cycleStageAfterMeRead, -1, 1790356109000)
	emitCycleStage(cycleStageAfterMeRead, 10000000, 1790356109000)
	emitCycleStage(cycleStageAfterMeRead, 1, 999999999999)
	emitCycleStage(cycleStageAfterMeRead, 1, 10000000000000)
	emitCycleStage(cycleStageAfterMeRead, 1, 0)

	emitVKStage("CACHE_HIT extra", 0, 1, 1790356109000, 0)
	emitVKStage("cAcHe_HiT", 0, 1, 1790356109000, 0)
	emitVKStage(vkStageCacheHit, -1, 1, 1790356109000, 0)
	emitVKStage(vkStageCacheHit, 1000, 1, 1790356109000, 0)
	emitVKStage(vkStageCacheHit, 0, -1, 1790356109000, 0)
	emitVKStage(vkStageCacheHit, 0, 10000000, 1790356109000, 0)
	emitVKStage(vkStageCacheHit, 0, 1, 999999999999, 0)
	emitVKStage(vkStageCacheHit, 0, 1, 10000000000000, 0)
	emitVKStage(vkStageCacheHit, 0, 1, 0, 0)
	// The token-specific detail field is bounded exactly like the elapsed field.
	emitVKStage(vkStageSerialLockWait, 0, 1, 1790356109000, -1)
	emitVKStage(vkStageSerialLockWait, 0, 1, 1790356109000, 10000000)

	if buf.Len() != 0 {
		t.Fatalf("invalid input must stay silent, got %q", buf.String())
	}
}

// The wait emitters must keep elapsed on the per-attempt base (comparable with every
// other vkstage line) and carry the measured wait in the separate detail field.
func TestDiagVKWaitKeepsPerAttemptElapsedAndBoundedDetail(t *testing.T) {
	var buf strings.Builder
	previous := diagStageDiagnostics
	diagStageDiagnostics = &buf
	defer func() { diagStageDiagnostics = previous }()
	diagAttempt.mu.Lock()
	previousBase := diagAttempt.base
	diagAttempt.base = time.Now()
	diagAttempt.mu.Unlock()
	defer func() {
		diagAttempt.mu.Lock()
		diagAttempt.base = previousBase
		diagAttempt.mu.Unlock()
	}()

	diagEmitVK(vkStageCacheMiss, 0)
	diagEmitVKWait(vkStageSerialLockWait, 0, 4321)
	diagEmitVKWait(vkStageThrottleWait, 0, 2500)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want three vkstage lines, got %q", buf.String())
	}
	waits := map[string]string{vkStageSerialLockWait: "4321", vkStageThrottleWait: "2500"}
	for _, line := range lines {
		match := vkStageLine.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("line outside the fixed grammar: %q", line)
		}
		elapsed, _ := strconv.ParseInt(match[3], 10, 64)
		if elapsed < 0 || elapsed >= 4321 {
			t.Fatalf("elapsed must be the small per-attempt value, got %d (%q)", elapsed, line)
		}
		wantDetail, isWait := waits[match[1]]
		if isWait && match[5] != wantDetail {
			t.Fatalf("wait %s detail: got %q want %q", match[1], match[5], wantDetail)
		}
		if !isWait && match[5] != "0" {
			t.Fatalf("non-wait %s must carry detail 0, got %q", match[1], match[5])
		}
	}
}

// The attempt base is monotonic and clamped: no attempt started means 0, and a long
// process keeps emitting valid numeric fields.
func TestDiagAttemptElapsedIsMonotonicAndClamped(t *testing.T) {
	diagAttempt.mu.Lock()
	previous := diagAttempt.base
	diagAttempt.mu.Unlock()
	defer func() {
		diagAttempt.mu.Lock()
		diagAttempt.base = previous
		diagAttempt.mu.Unlock()
	}()

	diagAttempt.mu.Lock()
	diagAttempt.base = time.Time{}
	diagAttempt.mu.Unlock()
	if got := diagElapsedMS(); got != 0 {
		t.Fatalf("no attempt base must yield 0, got %d", got)
	}

	diagMarkAttemptStart()
	first := diagElapsedMS()
	if first < 0 || first > diagElapsedMaxMS {
		t.Fatalf("elapsed outside the fixed field: %d", first)
	}
	if second := diagElapsedMS(); second < first {
		t.Fatalf("elapsed not monotonic: %d then %d", first, second)
	}

	diagAttempt.mu.Lock()
	diagAttempt.base = time.Now().Add(-3 * time.Hour)
	diagAttempt.mu.Unlock()
	if got := diagElapsedMS(); got != diagElapsedMaxMS {
		t.Fatalf("long attempt must clamp to %d, got %d", diagElapsedMaxMS, got)
	}
}

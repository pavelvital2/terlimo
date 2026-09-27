package accountaccess

// Fixed, secret-free chronology markers of the embedded session/auth path. The host
// mirror (NativeStderrMirror.kt) accepts exactly this vocabulary and writes
// `acctstage: TOKEN <elapsed_ms>` to logcat. Only the token and a monotonic elapsed
// value ever travel here: no URL, query, body, key, token or PII.
//
// The markers exist to distinguish, on a real run, where the challenge -> session
// boundary stalls: service response received, challenge decoded, host sign begin/end,
// session POST begin/response.

import (
	"fmt"
	"io"
	"os"
	"time"
)

const (
	stageAuthBegin               = "AUTH_BEGIN"
	stageServiceResponseReceived = "SERVICE_RESPONSE_RECEIVED"
	stageChallengeDecoded        = "CHALLENGE_DECODED"
	stageSignBegin               = "SIGN_BEGIN"
	stageSignEnd                 = "SIGN_END"
	stageSessionPostBegin        = "SESSION_POST_BEGIN"
	stageSessionResponse         = "SESSION_RESPONSE"
	stageSessionPostEnd          = "SESSION_POST_END"
	stageTokenUnknown            = "STAGE_UNKNOWN"
)

var stageTokenSet = map[string]bool{
	stageAuthBegin:               true,
	stageServiceResponseReceived: true,
	stageChallengeDecoded:        true,
	stageSignBegin:               true,
	stageSignEnd:                 true,
	stageSessionPostBegin:        true,
	stageSessionResponse:         true,
	stageSessionPostEnd:          true,
	stageTokenUnknown:            true,
}

// stageDiagnostics is the stderr sink of the fixed stage markers; focused tests
// substitute a buffer without changing behavior.
var stageDiagnostics io.Writer = os.Stderr

// stageElapsedCapMS bounds the numeric field so a line can never grow unbounded.
const stageElapsedCapMS = 3_600_000

// emitStage writes exactly one `acctstage: TOKEN <ms>` line and only ever emits a value
// from the fixed vocabulary; anything else collapses to STAGE_UNKNOWN.
func emitStage(token string, elapsed time.Duration) {
	if !stageTokenSet[token] {
		token = stageTokenUnknown
	}
	if elapsed < 0 {
		elapsed = 0
	}
	ms := elapsed.Milliseconds()
	if ms > stageElapsedCapMS {
		ms = stageElapsedCapMS
	}
	fmt.Fprintf(stageDiagnostics, "acctstage: %s %d\n", token, ms)
}

// stageBegin opens one authentication chronology and marks its start. The time.Time
// keeps its monotonic component so the elapsed value cannot jump on a wall-clock step.
func (m *MobileSession) stageBegin() {
	m.mu.Lock()
	m.stageStart = time.Now()
	m.mu.Unlock()
	emitStage(stageAuthBegin, 0)
}

// stage emits one fixed marker with the monotonic elapsed since stageBegin.
func (m *MobileSession) stage(token string) {
	m.mu.Lock()
	start := m.stageStart
	m.mu.Unlock()
	if start.IsZero() {
		emitStage(token, 0)
		return
	}
	emitStage(token, time.Since(start))
}

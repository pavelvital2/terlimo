package xyz.terlimo.test

import java.io.ByteArrayOutputStream
import java.io.IOException
import java.io.InputStream

/**
 * Native->host failures already travel on the bridge; this mirror surfaces only the fixed,
 * secret-free stderr contract lines to logcat for triage: the `accountaccess: CODE` codes,
 * the explicit onboarding-hour `onboarding: TOKEN` stages/terminals, the embedded
 * session/auth `acctstage:`/`svcstage:`/`plansdiag:` chronology and the per-attempt
 * `cyclestage:`/`vkstage:` chronology. Raw stderr, payloads,
 * tokens and keys never pass this gate, and the bridge diagnostic allowlist is not extended.
 * accountaccess values keep the historical bare-code format (the existing consumer contract);
 * onboarding tokens are source-qualified (`onboarding:...`) so both sources stay distinguishable.
 * At most [NativeStderrMirror]'s small per-window/total budget per native child; every
 * non-terminal line leaves the last slot of the window and of the total budget to a terminal.
 */
internal object NativeStderrCodes {
    const val TAG = "WDTT/NativeStderr"
    // The svctrace correlation line grew bounded optional fields (xid/elapsed/req/idle/err);
    // the cap stays a hard bound over a fixed grammar, never arbitrary text.
    const val MAX_LINE = 160
    private val ACCOUNT_ACCESS = Regex("^accountaccess: ([A-Z_]{1,64})$")
    private val ONBOARDING = Regex("^onboarding: ([A-Z_]{1,64})$")
    private val ACCTSTAGE = Regex("^acctstage: ([A-Z_]{1,64}) ([0-9]{1,7})$")
    private val SVCSTAGE = Regex("^svcstage: ([A-Z_]{1,64})$")
    private val SVCSTRACE = Regex("^svctrace: gen=([0-9]{1,10}) class=([A-Z_]{1,16}) event=([A-Z_]{1,16}) reused=([01]) port=([0-9]{1,5})(?: xid=([0-9]{1,10}))?(?: elapsed_ms=([0-9]{1,9}))?(?: req=([0-9]{1,3}))?(?: idle_ms=([0-9]{1,10}))?(?: err=([A-Z]{1,8}))?$")
    private val PLANSDIAG = Regex("^plansdiag: (BEGIN|OK|TRANSPORT|API_ERROR|NO_CLIENT)$")
    private val CYCLESTAGE = Regex("^cyclestage: ([A-Z0-9_]{1,64}) ([0-9]{1,7}) ([0-9]{13})$")
    private val VKSTAGE = Regex("^vkstage: ([A-Z0-9_]{1,64}) ([0-9]{1,3}) ([0-9]{1,7}) ([0-9]{13}) ([0-9]{1,7})$")
    private val VPNSOURCE = Regex("^vpnstage: (CANCEL_SOURCE|RUNTIME_CANCEL) ([A-Z_]{1,32})$")
    private val USAGESTAGE = Regex("^usagestage: ([A-Z0-9_]{1,40})$")
    private val REFRESH_RECEIVE = Regex("^refreshstage:receive:([0-9]{1,7})$")
    private val REFRESH_TOKEN = Regex("^refreshstage:(accepted|coalesced|manual_cycle_begin|manual_cycle_finish)$")
    val CODES = setOf(
        "SESSION_EXPIRED", "SESSION_INVALID", "PROOF_INVALID", "ACCESS_DENIED",
        "SUBSCRIPTION_MISSING", "SUBSCRIPTION_EXPIRED", "DEVICE_REVOKED", "RATE_LIMITED",
        "SERVICE_UNAVAILABLE", "BAD_MESSAGE", "TRANSPORT", "LEASE_SEQ_MISSING",
        "MOBILE_STATE_UNAVAILABLE", "REJECTED",
        // S3-A registration-stage diagnostics (fixed, secret-free).
        "REG_LINK_BEGIN", "REG_LINK_CLIENT_NIL", "REG_LINK_CTX", "REG_LINK_TRANSPORT",
        "REG_LINK_API_DISABLED", "REG_LINK_API_DENIED", "REG_LINK_API_OTHER",
        "REG_LINK_OK", "REG_LINK_REGISTERED", "REG_LINK_PENDING",
        // Registration failure classification (main budget; behavior-neutral).
        "REG_LINK_TIMEOUT", "REG_LINK_CANCELED", "REG_LINK_REPLY_FAILED",
        "REG_LINK_TRANSPORT_FAILED", "REG_LINK_SERVICE_UNAVAILABLE",
        "REG_LINK_SERVICE_PATH_DENIED", "REG_LINK_SERVICE_OTHER",
        "REG_LINK_SEED", "REG_LINK_PATH_REJECTED", "REG_LINK_REQUEST_REJECTED",
        "REG_LINK_ORIGIN_REJECTED", "REG_LINK_OTHER",
    )

    /** Fixed stage/terminal vocabulary of the explicit onboarding-hour path; mirrors the Go writer. */
    val ONBOARDING_TOKENS = setOf(
        "ENTRY", "INTENT_CHALLENGE", "INTENT_SIGN", "INTENT_POST", "INTENT_DECODE",
        "CHALLENGE_TRANSPORT", "CHALLENGE_STATUS_CLIENT", "CHALLENGE_STATUS_SERVER",
        "CHALLENGE_STATUS_OTHER", "CHALLENGE_DECODE", "CHALLENGE_SEMANTIC",
        "START_ENTRY", "START_NOT_READY", "START_UNAVAILABLE",
        "STATE_READY", "STARTED", "ONBOARDING_TIMEOUT", "ONBOARDING_FAILED",
        "ONBOARDING_PENDING_BUDGET", "ONBOARDING_START_NOT_READY", "ONBOARDING_START_UNAVAILABLE",
        "ONBOARDING_IN_FLIGHT", "ONBOARDING_CANCELLED", "ONBOARDING_CORRELATION",
        "ONBOARDING_EXPIRED", "ONBOARDING_REVOKED", "ONBOARDING_INTENT_CONFLICT",
        "ONBOARDING_UNKNOWN", "ONBOARDING_CREDENTIAL_UNAVAILABLE",
    )

    /** Stage tokens mark a failing explicit step; every other token is a terminal outcome. */
    val ONBOARDING_STAGES = setOf(
        "ENTRY", "INTENT_CHALLENGE", "INTENT_SIGN", "INTENT_POST", "INTENT_DECODE",
        "CHALLENGE_TRANSPORT", "CHALLENGE_STATUS_CLIENT", "CHALLENGE_STATUS_SERVER",
        "CHALLENGE_STATUS_OTHER", "CHALLENGE_DECODE", "CHALLENGE_SEMANTIC",
        "START_ENTRY", "START_NOT_READY", "START_UNAVAILABLE",
    )

    /**
     * Fixed chronology vocabulary of the embedded session/auth path (mirrors the Go writer):
     * `acctstage: TOKEN <elapsed_ms>`. Non-terminal; only the token and a bounded numeric
     * elapsed value pass, never a body/URL/key/token.
     */
    val ACCTSTAGE_TOKENS = setOf(
        "AUTH_BEGIN", "SERVICE_RESPONSE_RECEIVED", "CHALLENGE_DECODED",
        "SIGN_BEGIN", "SIGN_END", "SESSION_POST_BEGIN", "SESSION_RESPONSE",
        "SESSION_POST_END", "STAGE_UNKNOWN",
    )

    /**
     * Fixed per-attempt chronology vocabulary of one S5 mobile attempt (mirrors the Go
     * writer): `cyclestage: TOKEN <elapsed_ms> <utc_ms>`. Non-terminal; only the token and
     * two bounded integers pass, never a URL, status text, body, key or token.
     * `ME_RESPONSE_*` marks the wire response boundary of `GET /me` (the response returned
     * by the single request builder, before strict decode), while `AFTER_ME_READ` marks the
     * later Coordinator.Refresh return (post-decode): the gap between the two boundaries is
     * the decode/coordinator post-processing time.
     */
    val CYCLESTAGE_TOKENS = setOf(
        "ME_RESPONSE_2XX", "ME_RESPONSE_4XX", "ME_RESPONSE_5XX",
        "ME_RESPONSE_TRANSPORT", "ME_RESPONSE_OTHER",
        "AFTER_ME_READ", "EMIT_BEGIN", "EMIT_END_OK", "EMIT_END_ERR", "EMIT_END_CANCEL",
        "GW_REFRESH_BEGIN", "GW_PENDING_REFRESH_BEGIN", "GW_PENDING_REFRESH_END", "GW_REQUEST_BEGIN",
        "GW_REQUEST_END_2XX", "GW_REQUEST_END_4XX", "GW_REQUEST_END_5XX",
        "GW_REQUEST_END_TRANSPORT", "GW_REQUEST_END_OTHER",
        "ATTEMPT_RETRY_SLEEP", "ATTEMPT_RETRY_WAIT", "ATTEMPT_TERMINAL",
    )

    /**
     * Fixed bounded cold-VK substage vocabulary (mirrors the Go writer):
     * `vkstage: TOKEN <index> <elapsed_ms> <utc_ms> <detail_ms>`. Non-terminal; the index
     * is a bounded candidate/attempt number, [elapsed_ms] is the per-attempt elapsed since
     * the attempt start (the same meaning as on every cyclestage line) and [detail_ms] is a
     * token-specific bounded duration (the measured wait for SERIAL_LOCK_WAIT/THROTTLE_WAIT,
     * 0 elsewhere). Never a hash, credential, host, link, IP or other value.
     */
    val VKSTAGE_TOKENS = setOf(
        "CACHE_HIT", "CACHE_MISS", "SERIAL_LOCK_WAIT", "THROTTLE_WAIT",
        "FETCH_BEGIN", "FETCH_END_OK", "FETCH_END_ERR",
        "PROVIDER_MODERN", "PROVIDER_LEGACY", "HASH_BEGIN", "HASH_END",
    )

    /** Fixed transport boundary outcomes. No endpoint, frame ID, body or credentials. */
    val SVCSTAGE_TOKENS = setOf(
        "LOCAL_REJECTED", "ESTABLISH_BEGIN", "ESTABLISH_OK", "ESTABLISH_FAILED",
        "ESTABLISH_SEED_READY", "ESTABLISH_VK_BEGIN", "ESTABLISH_VK_READY",
        "ESTABLISH_DIAL_BEGIN", "ESTABLISH_DIAL_READY",
        "ESTABLISH_TIMEOUT_UNPROCESSED", "ESTABLISH_CANCEL_UNPROCESSED",
        "FRAME_WRITE_BEGIN", "FRAME_WRITE_OK", "FRAME_WRITE_FAILED",
        "FRAME_READ_BEGIN", "FRAME_READ_OK", "FRAME_READ_FAILED", "FRAME_INVALID",
        "SERVICE_REPLY_BEGIN", "SERVICE_REPLY_OK", "SERVICE_REPLY_FAILED", "SERVICE_ERROR",
        // Machine-readable bounded peer error codes (unknown collapses to OTHER natively).
        "SERVICE_ERROR_SERVICE_BAD_FRAME", "SERVICE_ERROR_SERVICE_BAD_METHOD",
        "SERVICE_ERROR_SERVICE_BAD_PATH", "SERVICE_ERROR_SERVICE_PATH_DENIED",
        "SERVICE_ERROR_SERVICE_BAD_HEADERS", "SERVICE_ERROR_SERVICE_BUSY",
        "SERVICE_ERROR_SERVICE_UNAVAILABLE", "SERVICE_ERROR_SERVICE_SEED_STALE",
        "SERVICE_ERROR_SERVICE_SEED_BINDING", "SERVICE_ERROR_SERVICE_BAD_RESPONSE",
        "SERVICE_ERROR_OTHER",
        // Bounded native controller run-exit outcome/source (fixed enums, no raw error).
        "RUN_EXIT_OK", "RUN_EXIT_CANCELED", "RUN_EXIT_ERROR",
        "RUN_STOP_SOURCE_STDIN_EOF", "RUN_STOP_SOURCE_STDIN_ERROR",
        "RUN_STOP_SOURCE_CTX_ALREADY_CANCELED", "RUN_STOP_SOURCE_EXPLICIT_CANCEL",
        "RUN_STOP_SOURCE_SIGNAL_TERM", "RUN_STOP_SOURCE_SIGNAL_INT",
        "RUN_STOP_SOURCE_SIGNAL_OTHER", "RUN_STOP_SOURCE_CONTROLLER_RETURN",
        "RUN_STOP_SOURCE_CANCELED_OTHER", "RUN_STOP_SOURCE_UNKNOWN",
        "EXCHANGE_CANCELLED", "EXCHANGE_TIMEOUT", "CONNECTION_CLOSED",
        // Bounded local-reject reasons of the service Doer (no URL/header/secret).
        "REJECT_METHOD", "REJECT_ORIGIN", "REJECT_PATH", "REJECT_HEADERS",
        "REJECT_BODY", "REJECT_SEED", "REJECT_FRAME",
        "REJECT_PATH_EMPTY_OR_LONG", "REJECT_PATH_CHARS", "REJECT_PATH_DOTS",
        "REJECT_PATH_DOUBLESLASH", "REJECT_PATH_QUERY", "REJECT_PATH_NOT_ALLOWED",
    )

    /** Fixed first-cause vocabulary of the VPN preparation cancellation (sanitized). */
    val VPNSOURCE_TOKENS = setOf(
        "HOST_STOP", "CONNECT_BUDGET", "SWITCH", "REVOKED", "PARENT_SHUTDOWN",
        "WORKER_GATE", "WORKER_CREDS", "WORKER_TERMINAL", "UNKNOWN",
    )

    /** Fixed /usage HTTP-class and first-failure-cause vocabulary (mirrors the Go writer). */
    val USAGESTAGE_TOKENS = setOf(
        "RESPONSE_2XX", "RESPONSE_4XX", "RESPONSE_5XX", "RESPONSE_OTHER",
        "FAIL_LOCAL_SEED_MISSING", "FAIL_LOCAL_ORIGIN_REJECTED", "FAIL_LOCAL_PATH_REJECTED",
        "FAIL_LOCAL_REQUEST_REJECTED", "FAIL_SERVICE_RESPONSE_REJECTED", "FAIL_SERVICE_PATH_DENIED",
        "FAIL_SERVICE_UNAVAILABLE", "FAIL_SERVICE_ERROR", "FAIL_TRANSPORT_TIMEOUT",
        "FAIL_TRANSPORT_FAILED", "FAIL_TRUST_FAILED", "FAIL_VK_API_UNAVAILABLE", "FAIL_TRANSPORT",
    )

    /** Fixed §11/§29 manual-refresh chronology vocabulary (mirrors the Go writer). */
    val REFRESHSTAGE_TOKENS = setOf(
        "receive", "accepted", "coalesced", "manual_cycle_begin", "manual_cycle_finish",
    )

    /** Fixed request classes and event vocabulary of the secret-free svctrace correlation. */
    val SVCSTRACE_CLASSES = setOf("AUTH", "ME", "GATEWAYS", "ACCESS_SYNC", "REG_LINK", "PLANS", "USAGE", "OTHER")
    val SVCSTRACE_EVENTS = setOf(
        "ESTABLISH_OK", "ESTABLISH_FAIL", "WRITE_BEGIN", "WRITE_OK", "WRITE_FAIL",
        "READ_BEGIN", "READ_OK", "READ_FAIL", "EXCHANGE_END",
        "REUSE_STALE_REQ", "REUSE_STALE_IDLE", "REUSE_STALE_SEED",
    )

    /** Fixed error-class vocabulary of the svctrace correlation (never raw error text). */
    val SVCSTRACE_ERR = setOf("EOF", "CLOSED", "TIMEOUT", "CANCELED", "RESET", "REFUSED", "OTHER")

    // Per-dial native buffer: at most 64 events + one FINISH. No payload fields.
    private val DIALSTAGE = Regex("^dialstage: call=([1-9][0-9]{0,19}) candidate=(0|[1-9][0-9]{0,18}) transport=(NONE|UDP|TCP|TLS) stage=([A-Z_]{1,24}) result=(BEGIN|OK|CANCELED|TIMEOUT|EOF|CLOSED|OTHER) elapsed_ms=([0-9]{1,19})(?: truncated=([01]))?$")
    private val DIALSTAGE_TOKENS = setOf(
        "SOCKET_BEGIN", "SOCKET_END", "TLS_BEGIN", "TLS_END", "CLIENT_BEGIN", "CLIENT_END",
        "ALLOCATE_BEGIN", "ALLOCATE_END", "CERT_BEGIN", "CERT_END", "SEMAPHORE_WAIT",
        "SEMAPHORE_ACQUIRED", "SEMAPHORE_END", "DTLS_BEGIN", "DTLS_END",
        "FIRST_WRITE_BEGIN", "FIRST_WRITE_END", "CANDIDATE_BEGIN", "CANDIDATE_END", "FINISH",
    )

    /** One exact allowlisted line: its observable value and whether it owns the reserved slot. */
    data class Match(val value: String, val terminal: Boolean)

    /**
     * Exact allowlisted line only; null for anything else. accountaccess returns the historical
     * bare code (unchanged consumer format); onboarding returns the source-qualified
     * `onboarding:TOKEN`. accountaccess codes are never terminal: the reserved final slot
     * belongs to the fixed onboarding terminal vocabulary only.
     */
    fun match(line: String): Match? {
        if (line.startsWith("dialstage: ")) {
            if (line.length > 256) return null
            val m = DIALSTAGE.matchEntire(line) ?: return null
            val stage = m.groupValues[4]
            val result = m.groupValues[5]
            val finish = stage == "FINISH"
            val begin = stage.endsWith("_BEGIN") || stage == "SEMAPHORE_WAIT"
            if (stage !in DIALSTAGE_TOKENS || begin != (result == "BEGIN")) return null
            if (finish != m.groupValues[7].isNotEmpty()) return null
            if (finish != (m.groupValues[2] == "0" && m.groupValues[3] == "NONE")) return null
            if (!finish && (m.groupValues[2] == "0" || m.groupValues[3] == "NONE")) return null
            if (stage == "SEMAPHORE_ACQUIRED" && result != "OK") return null
            return Match("dialstage:call=${m.groupValues[1]}:candidate=${m.groupValues[2]}:" +
                "transport=${m.groupValues[3]}:stage=$stage:result=$result:elapsed_ms=${m.groupValues[6]}" +
                (if (finish) ":truncated=${m.groupValues[7]}" else ""), terminal = false)
        }
        if (line.length > MAX_LINE) return null
        ACCOUNT_ACCESS.matchEntire(line)?.groupValues?.get(1)?.takeIf { it in CODES }
            ?.let { return Match(it, terminal = false) }
        ONBOARDING.matchEntire(line)?.groupValues?.get(1)?.takeIf { it in ONBOARDING_TOKENS }
            ?.let { return Match("onboarding:$it", terminal = it !in ONBOARDING_STAGES) }
        ACCTSTAGE.matchEntire(line)?.let { m ->
            val token = m.groupValues[1]
            if (token in ACCTSTAGE_TOKENS) return Match("acctstage:$token:${m.groupValues[2]}", terminal = false)
        }
        CYCLESTAGE.matchEntire(line)?.let { m ->
            val token = m.groupValues[1]
            if (token in CYCLESTAGE_TOKENS) {
                return Match("cyclestage:$token:${m.groupValues[2]}:${m.groupValues[3]}", terminal = false)
            }
        }
        VKSTAGE.matchEntire(line)?.let { m ->
            val token = m.groupValues[1]
            if (token in VKSTAGE_TOKENS) {
                return Match(
                    "vkstage:$token:${m.groupValues[2]}:${m.groupValues[3]}:${m.groupValues[4]}:${m.groupValues[5]}",
                    terminal = false,
                )
            }
        }
        VPNSOURCE.matchEntire(line)?.let { m ->
            val kind = m.groupValues[1]
            val token = m.groupValues[2]
            if (token in VPNSOURCE_TOKENS) return Match("vpnstage:$kind:$token", terminal = false)
        }
        USAGESTAGE.matchEntire(line)?.groupValues?.get(1)?.takeIf { it in USAGESTAGE_TOKENS }
            ?.let { return Match("usagestage:$it", terminal = false) }
        SVCSTAGE.matchEntire(line)?.groupValues?.get(1)?.takeIf { it in SVCSTAGE_TOKENS }
            ?.let { return Match("svcstage:$it", terminal = false) }
        PLANSDIAG.matchEntire(line)?.groupValues?.get(1)
            ?.let { return Match("plansdiag:$it", terminal = false) }
        REFRESH_RECEIVE.matchEntire(line)?.let { m ->
            return Match("refreshstage:receive:${m.groupValues[1]}", terminal = false)
        }
        REFRESH_TOKEN.matchEntire(line)?.let { m ->
            return Match("refreshstage:${m.groupValues[1]}", terminal = false)
        }
        SVCSTRACE.matchEntire(line)?.let { m ->
            val gen = m.groupValues[1]
            val cls = m.groupValues[2]
            val event = m.groupValues[3]
            val reused = m.groupValues[4]
            val port = m.groupValues[5].toIntOrNull()
            val err = m.groupValues[10]
            if (cls in SVCSTRACE_CLASSES && event in SVCSTRACE_EVENTS && port != null && port in 0..65535 &&
                (err.isEmpty() || err in SVCSTRACE_ERR)) {
                val builder = StringBuilder("svctrace:gen=$gen:class=$cls:event=$event:reused=$reused:port=$port")
                if (m.groupValues[6].isNotEmpty()) builder.append(":xid=").append(m.groupValues[6])
                if (m.groupValues[7].isNotEmpty()) builder.append(":elapsed_ms=").append(m.groupValues[7])
                if (m.groupValues[8].isNotEmpty()) builder.append(":req=").append(m.groupValues[8])
                if (m.groupValues[9].isNotEmpty()) builder.append(":idle_ms=").append(m.groupValues[9])
                if (err.isNotEmpty()) builder.append(":err=").append(err)
                return Match(builder.toString(), terminal = false)
            }
        }
        return null
    }

    /** Mirror-eligible observable value only for an exact allowlisted line; null otherwise. */
    fun code(line: String): String? = match(line)?.value
}

/**
 * Bounded rate: a burst cannot flood logcat. One instance per native child, single drain thread.
 * The accepted hard limits are [maxPerWindow] lines per [windowNanos] and [maxTotal] per child
 * and are never raised. The reservation is based on the global budget usage, not per source: the
 * last slot of the window and of the total budget stays reserved for a terminal onboarding
 * outcome, so every non-terminal line (accountaccess codes and onboarding stages alike) is
 * admitted only while the window holds fewer than [maxPerWindow] - 1 lines and the child fewer
 * than [maxTotal] - 1 lines. Non-terminal throughput is therefore tightened to 7 lines / 10 s and
 * 63 lines total under the production limits, while a terminal may still spend the final slot and
 * is itself bounded by the same hard window cap. Honest bound: once the hard total cap is
 * exhausted no further line can be emitted, terminals included.
 *
 * The fixed `acctstage:`, `svcstage:` and `plansdiag:` chronologies share the dedicated
 * bounded stage budget ([maxStagePerWindow] / [maxStageTotal]) so a failed preferred round
 * plus its fallback still fit without evicting an accountaccess/onboarding line and without
 * touching the terminal reservation. `cyclestage:` and `vkstage:` each own a separate bounded
 * budget ([maxCyclePerWindow] / [maxCycleTotal] and [maxVkPerWindow] / [maxVkTotal]): a cold
 * VK prelude cannot fill the shared stage budget before the critical post-/me markers, and no
 * class can evict another. Every stage class is non-terminal: none ever spends the reserved
 * terminal slot. Worst case per child with the production defaults:
 * [maxTotal] 64 + [maxStageTotal] 64 + [maxCycleTotal] 48 + [maxVkTotal] 24 + [maxTraceTotal]
 * 4096 = 4296 mirrored lines.
 *
 * Mobile `dialstage:` has an independent 65-lines/window, 260-lines/child budget.
 * One native capped batch (64 events + FINISH) fits without taking any old budget.
 * Later batches can still be rate-limited: a missing FINISH means incomplete capture.
 *
 * The §11/§29 manual-refresh chronology (`refreshstage:`) owns its own small bounded budget
 * ([maxRefreshPerWindow] / [maxRefreshTotal]) so a rapid tap pair (receive/accepted/coalesced/
 * manual_cycle_begin/finish) always fits without evicting or being evicted by the other streams.
 */
internal class NativeStderrMirror(
    private val maxPerWindow: Int = 8,
    private val windowNanos: Long = 10_000_000_000L,
    private val maxTotal: Int = 64,
    private val maxStagePerWindow: Int = 16,
    private val maxStageTotal: Int = 64,
    private val maxCyclePerWindow: Int = 16,
    private val maxCycleTotal: Int = 48,
    private val maxVkPerWindow: Int = 8,
    private val maxVkTotal: Int = 24,
    private val maxUsagePerWindow: Int = 8,
    private val maxUsageTotal: Int = 32,
    private val maxRefreshPerWindow: Int = 16,
    private val maxRefreshTotal: Int = 32,
    private val maxTracePerWindow: Int = 64,
    private val maxTraceTotal: Int = 4096,
    // Independent budget: one entire bounded dial burst fits; old caps unchanged.
    private val maxDialPerWindow: Int = 65,
    private val maxDialTotal: Int = 260,
) {
    private var windowStartNanos = 0L
    private var windowCount = 0
    private var total = 0
    private var stageWindowCount = 0
    private var stageTotal = 0
    private var cycleWindowCount = 0
    private var cycleTotal = 0
    private var vkWindowCount = 0
    private var vkTotal = 0
    private var usageWindowCount = 0
    private var usageTotal = 0
    private var refreshWindowCount = 0
    private var refreshTotal = 0
    private var traceWindowCount = 0
    private var traceTotal = 0
    private var dialWindowCount = 0
    private var dialTotal = 0

    fun accept(line: String, nowNanos: Long = System.nanoTime()): String? {
        val match = NativeStderrCodes.match(line) ?: return null
        if (nowNanos - windowStartNanos >= windowNanos) {
            windowStartNanos = nowNanos
            windowCount = 0
            stageWindowCount = 0
            cycleWindowCount = 0
            vkWindowCount = 0
            usageWindowCount = 0
            refreshWindowCount = 0
            traceWindowCount = 0
            dialWindowCount = 0
        }
        if (match.value.startsWith("dialstage:")) {
            if (dialTotal >= maxDialTotal || dialWindowCount >= maxDialPerWindow) return null
            dialWindowCount++
            dialTotal++
            return match.value
        }
        if (match.value.startsWith(TRACE_PREFIX)) {
            // Dedicated bounded correlation budget so session/class/port events are never
            // evicted by other streams; still bounded to protect logcat.
            if (traceTotal >= maxTraceTotal) return null
            if (traceWindowCount >= maxTracePerWindow) return null
            traceWindowCount++
            traceTotal++
            return match.value
        }
        if (match.value.startsWith(CYCLE_STAGE_PREFIX)) {
            // Dedicated bounded cycle chronology; the cold-VK prelude and the shared stage
            // chronologies cannot evict the critical post-/me markers and vice versa.
            if (cycleTotal >= maxCycleTotal) return null
            if (cycleWindowCount >= maxCyclePerWindow) return null
            cycleWindowCount++
            cycleTotal++
            return match.value
        }
        if (match.value.startsWith(VK_STAGE_PREFIX)) {
            // Dedicated bounded cold-VK chronology; the accountaccess/onboarding budget, the
            // shared stage budget and the reserved terminal slot are untouched by construction.
            if (vkTotal >= maxVkTotal) return null
            if (vkWindowCount >= maxVkPerWindow) return null
            vkWindowCount++
            vkTotal++
            return match.value
        }
        if (match.value.startsWith(REFRESH_STAGE_PREFIX)) {
            // §11/§29 manual-refresh chronology: a rapid tap pair must always fit and must not
            // evict (or be evicted by) any other diagnostic stream.
            if (refreshTotal >= maxRefreshTotal) return null
            if (refreshWindowCount >= maxRefreshPerWindow) return null
            refreshWindowCount++
            refreshTotal++
            return match.value
        }
        if (match.value.startsWith(USAGE_STAGE_PREFIX)) {
            // Dedicated bounded /usage diagnostic budget: the per-sweep usagestage stream must
            // never consume the general budget and evict the vpnstage producer markers
            // (CANCEL_SOURCE / RUNTIME_CANCEL) that locate a runtime stop.
            if (usageTotal >= maxUsageTotal) return null
            if (usageWindowCount >= maxUsagePerWindow) return null
            usageWindowCount++
            usageTotal++
            return match.value
        }
        if (match.value.startsWith(ACCT_STAGE_PREFIX) || match.value.startsWith(SERVICE_STAGE_PREFIX) ||
            match.value.startsWith(PLANS_DIAG_PREFIX)) {
            // Bounded dedicated chronology budget; the accountaccess/onboarding budget and the
            // reserved terminal slot are untouched by construction.
            if (stageTotal >= maxStageTotal) return null
            if (stageWindowCount >= maxStagePerWindow) return null
            stageWindowCount++
            stageTotal++
            return match.value
        }
        if (total >= maxTotal) return null
        if (windowCount >= maxPerWindow) return null
        if (!match.terminal) {
            // Leave the last global slot of this window and of the total budget for a terminal.
            if (windowCount >= maxPerWindow - 1) return null
            if (total >= maxTotal - 1) return null
        }
        windowCount++
        total++
        return match.value
    }

    private companion object {
        const val ACCT_STAGE_PREFIX = "acctstage:"
        const val SERVICE_STAGE_PREFIX = "svcstage:"
        const val TRACE_PREFIX = "svctrace:"
        const val PLANS_DIAG_PREFIX = "plansdiag:"
        const val CYCLE_STAGE_PREFIX = "cyclestage:"
        const val VK_STAGE_PREFIX = "vkstage:"
        const val USAGE_STAGE_PREFIX = "usagestage:"
        const val REFRESH_STAGE_PREFIX = "refreshstage:"
    }
}

/**
 * Drains child stderr line by line and emits only mirror-eligible fixed code tokens. Every other
 * byte/line is discarded in bounded space; nothing else is logged or persisted.
 */
internal fun drainNativeStderr(input: InputStream, onCode: (String) -> Unit) {
    val mirror = NativeStderrMirror()
    try {
        input.use {
            val line = ByteArrayOutputStream()
            var overflow = false
            while (true) {
                val byte = it.read()
                if (byte < 0) break
                if (byte == '\n'.code) {
                    if (!overflow) mirror.accept(line.toString("UTF-8"))?.let(onCode)
                    line.reset()
                    overflow = false
                } else if (overflow) {
                    // This overlong line keeps being discarded without buffering it.
                } else if (line.size() >= MAX_RAW_LINE) {
                    line.reset()
                    overflow = true
                } else {
                    line.write(byte)
                }
            }
            if (!overflow && line.size() > 0) mirror.accept(line.toString("UTF-8"))?.let(onCode)
        }
    } catch (_: IOException) {
        // stdout/bridge owns process failure reporting; stderr close must not crash the host.
    }
}

private const val MAX_RAW_LINE = 4096

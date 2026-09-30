package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class NativeStderrMirrorTest {
    @Test fun mirrorsOnlyExactAllowlistedContractLines() {
        // accountaccess keeps the historical bare-code value: the existing consumer format.
        assertEquals("SUBSCRIPTION_MISSING", NativeStderrCodes.code("accountaccess: SUBSCRIPTION_MISSING"))
        assertEquals("ACCESS_DENIED", NativeStderrCodes.code("accountaccess: ACCESS_DENIED"))
        assertEquals("MOBILE_STATE_UNAVAILABLE", NativeStderrCodes.code("accountaccess: MOBILE_STATE_UNAVAILABLE"))
        assertNull(NativeStderrCodes.code("accountaccess: NOT_IN_ALLOWLIST"))
        assertNull(NativeStderrCodes.code("accountaccess: subcription_missing"))
        assertNull(NativeStderrCodes.code(" accountaccess: SUBSCRIPTION_MISSING"))
        assertNull(NativeStderrCodes.code("accountaccess: SUBSCRIPTION_MISSING extra"))
        assertNull(NativeStderrCodes.code("accountaccess:SUBSCRIPTION_MISSING"))
        assertNull(NativeStderrCodes.code("SUBSCRIPTION_MISSING"))
        assertNull(NativeStderrCodes.code("accountaccess: SUBSCRIPTION MISSING"))
        // Restored format is exactly the bare code, never source-qualified.
        val restored = NativeStderrCodes.code("accountaccess: TRANSPORT")
        assertEquals("TRANSPORT", restored)
        assertFalse(restored!!.contains(":"))
        assertFalse(restored.startsWith("accountaccess:"))
    }

    @Test fun acceptsOnlyFixedOnboardingTokens() {
        assertEquals("onboarding:ENTRY", NativeStderrCodes.code("onboarding: ENTRY"))
        assertEquals("onboarding:START_ENTRY", NativeStderrCodes.code("onboarding: START_ENTRY"))
        assertEquals("onboarding:ONBOARDING_START_NOT_READY",
            NativeStderrCodes.code("onboarding: ONBOARDING_START_NOT_READY"))
        assertEquals("onboarding:ONBOARDING_PENDING_BUDGET",
            NativeStderrCodes.code("onboarding: ONBOARDING_PENDING_BUDGET"))
        assertEquals("onboarding:ONBOARDING_UNKNOWN", NativeStderrCodes.code("onboarding: ONBOARDING_UNKNOWN"))
        assertEquals("onboarding:ONBOARDING_CREDENTIAL_UNAVAILABLE", NativeStderrCodes.code("onboarding: ONBOARDING_CREDENTIAL_UNAVAILABLE"))
        // Fixed challenge-failure classes (non-terminal, source-qualified).
        listOf("CHALLENGE_TRANSPORT", "CHALLENGE_STATUS_CLIENT", "CHALLENGE_STATUS_SERVER",
            "CHALLENGE_STATUS_OTHER", "CHALLENGE_DECODE", "CHALLENGE_SEMANTIC").forEach {
            assertEquals("onboarding:$it", NativeStderrCodes.code("onboarding: $it"))
        }
        assertNull(NativeStderrCodes.code("onboarding: CHALLENGE_STATUS_403"))
        assertNull(NativeStderrCodes.code("onboarding: challenge_transport"))
        // Raw backend codes/statuses, arbitrary words and credential-looking lines never pass.
        assertNull(NativeStderrCodes.code("onboarding: ONBOARDING_START_CONFLICT"))
        assertNull(NativeStderrCodes.code("onboarding: HTTP409 ONBOARDING_START_CONFLICT"))
        assertNull(NativeStderrCodes.code("onboarding: HTTP500"))
        assertNull(NativeStderrCodes.code("onboarding: timeout"))
        assertNull(NativeStderrCodes.code("onboarding: ONBOARDING_TIMEOUT extra"))
        assertNull(NativeStderrCodes.code("onboarding:Bearer sekrit"))
        assertNull(NativeStderrCodes.code("onboarding: " + "A".repeat(65)))
        assertNull(NativeStderrCodes.code("onboarding: secret-ready-secret"))
        // Source separation: onboarding tokens are prefixed, accountaccess values stay bare.
        assertTrue(NativeStderrCodes.code("onboarding: ENTRY")!!.startsWith("onboarding:"))
        assertFalse(NativeStderrCodes.code("accountaccess: REJECTED")!!.startsWith("accountaccess:"))
        assertEquals("REJECTED", NativeStderrCodes.code("accountaccess: REJECTED"))
    }

    @Test fun acceptsFixedAcctStageChronologyWithoutSecrets() {
        assertEquals("acctstage:AUTH_BEGIN:0", NativeStderrCodes.code("acctstage: AUTH_BEGIN 0"))
        assertEquals("acctstage:SERVICE_RESPONSE_RECEIVED:9012",
            NativeStderrCodes.code("acctstage: SERVICE_RESPONSE_RECEIVED 9012"))
        listOf("CHALLENGE_DECODED", "SIGN_BEGIN", "SIGN_END", "SESSION_POST_BEGIN",
            "SESSION_RESPONSE", "STAGE_UNKNOWN").forEach {
            assertEquals("acctstage:$it:123", NativeStderrCodes.code("acctstage: $it 123"))
        }
        assertNull(NativeStderrCodes.code("acctstage: NOT_A_STAGE 1"))
        assertNull(NativeStderrCodes.code("acctstage: auth_begin 1"))
        assertNull(NativeStderrCodes.code("acctstage: AUTH_BEGIN"))
        assertNull(NativeStderrCodes.code("acctstage: AUTH_BEGIN -1"))
        assertNull(NativeStderrCodes.code("acctstage: AUTH_BEGIN 99999999999"))
        assertNull(NativeStderrCodes.code("acctstage: AUTH_BEGIN 1 https://host/path?token=sekrit"))
        assertNull(NativeStderrCodes.code("acctstage: SIGN_BEGIN 10 token=sekrit"))
        assertNull(NativeStderrCodes.code("acctstage:AUTH_BEGIN:0"))
    }

    @Test fun acceptsFixedCycleAndVkStageChronologyWithoutSecrets() {
        assertEquals("cyclestage:AFTER_ME_READ:123:1790356109000",
            NativeStderrCodes.code("cyclestage: AFTER_ME_READ 123 1790356109000"))
        // ME_RESPONSE_* is the wire READ boundary of GET /me; AFTER_ME_READ stays post-decode.
        listOf("ME_RESPONSE_2XX", "ME_RESPONSE_4XX", "ME_RESPONSE_5XX",
            "ME_RESPONSE_TRANSPORT", "ME_RESPONSE_OTHER").forEach {
            assertEquals("cyclestage:$it:0:1790356109000",
                NativeStderrCodes.code("cyclestage: $it 0 1790356109000"))
        }
        listOf("EMIT_BEGIN", "EMIT_END_OK", "EMIT_END_ERR", "EMIT_END_CANCEL",
            "GW_REFRESH_BEGIN", "GW_PENDING_REFRESH_BEGIN", "GW_REQUEST_BEGIN",
            "GW_REQUEST_END_2XX", "GW_REQUEST_END_4XX", "GW_REQUEST_END_5XX",
            "GW_REQUEST_END_TRANSPORT", "GW_REQUEST_END_OTHER").forEach {
            assertEquals("cyclestage:$it:9999999:9999999999999",
                NativeStderrCodes.code("cyclestage: $it 9999999 9999999999999"))
        }
        // vkstage carries four bounded numbers: index, per-attempt elapsed, UTC and detail.
        assertEquals("vkstage:CACHE_HIT:0:5:1790356109000:0",
            NativeStderrCodes.code("vkstage: CACHE_HIT 0 5 1790356109000 0"))
        assertEquals("vkstage:SERIAL_LOCK_WAIT:0:5:1790356109000:4321",
            NativeStderrCodes.code("vkstage: SERIAL_LOCK_WAIT 0 5 1790356109000 4321"))
        listOf("CACHE_MISS", "SERIAL_LOCK_WAIT", "THROTTLE_WAIT", "FETCH_BEGIN",
            "FETCH_END_OK", "FETCH_END_ERR", "PROVIDER_MODERN", "PROVIDER_LEGACY",
            "HASH_BEGIN", "HASH_END").forEach {
            assertEquals("vkstage:$it:999:7:1000000000000:0",
                NativeStderrCodes.code("vkstage: $it 999 7 1000000000000 0"))
        }
        // Unknown token, wrong case, extra fields, wrong numeric width and negatives are dropped.
        assertNull(NativeStderrCodes.code("cyclestage: NOT_A_STAGE 123 1790356109000"))
        assertNull(NativeStderrCodes.code("cyclestage: after_me_read 123 1790356109000"))
        assertNull(NativeStderrCodes.code("cyclestage: ME_RESPONSE_403 0 1790356109000"))
        assertNull(NativeStderrCodes.code("cyclestage: GW_REQUEST_END_2XX 123 1790356109000 extra"))
        assertNull(NativeStderrCodes.code("cyclestage: GW_REQUEST_END_2XX 10000000 1790356109000"))
        assertNull(NativeStderrCodes.code("cyclestage: GW_REQUEST_END_2XX 123 999999999999"))
        assertNull(NativeStderrCodes.code("cyclestage: GW_REQUEST_END_2XX 123 10000000000000"))
        assertNull(NativeStderrCodes.code("cyclestage: GW_REQUEST_END_2XX -1 1790356109000"))
        assertNull(NativeStderrCodes.code("cyclestage: EMIT_BEGIN 1 1790356109000 token=sekrit"))
        assertNull(NativeStderrCodes.code("vkstage: CACHE_HIT 0 5 1790356109000 extra"))
        assertNull(NativeStderrCodes.code("vkstage: CACHE_HIT 1000 5 1790356109000 0"))
        assertNull(NativeStderrCodes.code("vkstage: CACHE_HIT 0 5"))
        assertNull(NativeStderrCodes.code("vkstage: CACHE_HIT -1 5 1790356109000 0"))
        assertNull(NativeStderrCodes.code("vkstage: CACHE_HIT 0 5 17903561090000 0"))
        // Credential/hash-looking content never passes: overlong lines and non-token classes.
        assertNull(NativeStderrCodes.code("cyclestage: " + "A".repeat(80) + " 123 1790356109000"))
        assertNull(NativeStderrCodes.code("vkstage: " + "ab12cd34".repeat(8) + " 0 5 1790356109000 0"))
    }

    @Test fun rejectsMalformedVkStageDetailField() {
        // The detail field is mandatory and bounded; a missing 4th field, extra field,
        // non-numeric or out-of-range detail and unknown tokens are all dropped.
        assertNull(NativeStderrCodes.code("vkstage: CACHE_HIT 0 5 1790356109000"))
        assertNull(NativeStderrCodes.code("vkstage: CACHE_HIT 0 5 1790356109000 0 extra"))
        assertNull(NativeStderrCodes.code("vkstage: CACHE_HIT 0 5 1790356109000 abc"))
        assertNull(NativeStderrCodes.code("vkstage: CACHE_HIT 0 5 1790356109000 -1"))
        assertNull(NativeStderrCodes.code("vkstage: CACHE_HIT 0 5 1790356109000 10000000"))
        assertNull(NativeStderrCodes.code("vkstage: NOT_A_STAGE 0 5 1790356109000 0"))
    }

    @Test fun cycleAndVkStagesUseTheirOwnBudgetsWithoutSpendingTheTerminalSlot() {
        val mirror = NativeStderrMirror(maxStagePerWindow = 2, maxStageTotal = 3,
            maxCyclePerWindow = 2, maxCycleTotal = 3, maxVkPerWindow = 1, maxVkTotal = 2)
        assertEquals("cyclestage:AFTER_ME_READ:0:1790356109000",
            mirror.accept("cyclestage: AFTER_ME_READ 0 1790356109000", 0))
        assertEquals("vkstage:CACHE_MISS:0:5:1790356109000:0",
            mirror.accept("vkstage: CACHE_MISS 0 5 1790356109000 0", 0))
        // Cycle and VK stop at their own per-window bounds...
        assertEquals("cyclestage:GW_REQUEST_BEGIN:9:1790356109000",
            mirror.accept("cyclestage: GW_REQUEST_BEGIN 9 1790356109000", 0))
        assertNull(mirror.accept("cyclestage: EMIT_BEGIN 9 1790356109000", 0))
        assertNull(mirror.accept("vkstage: HASH_BEGIN 1 5 1790356109000 0", 0))
        // ...and they do not share the generic stage budget with acctstage/svcstage.
        assertEquals("acctstage:SIGN_BEGIN:5", mirror.accept("acctstage: SIGN_BEGIN 5", 0))
        assertEquals("svcstage:ESTABLISH_BEGIN", mirror.accept("svcstage: ESTABLISH_BEGIN", 0))
        assertNull(mirror.accept("acctstage: SIGN_END 5", 0))
        // The main budget still keeps its reserved terminal slot untouched.
        assertEquals("onboarding:ONBOARDING_FAILED", mirror.accept("onboarding: ONBOARDING_FAILED", 0))
        // A new window opens class slots again, but each class total stays honest.
        assertEquals("vkstage:HASH_BEGIN:1:5:1790356109000:0",
            mirror.accept("vkstage: HASH_BEGIN 1 5 1790356109000 0", 10_000_000_000L))
        assertNull(mirror.accept("vkstage: CACHE_HIT 0 5 1790356109000 0", 10_000_000_000L))
        assertEquals("cyclestage:GW_REQUEST_END_4XX:9:1790356109000",
            mirror.accept("cyclestage: GW_REQUEST_END_4XX 9 1790356109000", 10_000_000_000L))
        assertNull(mirror.accept("cyclestage: EMIT_BEGIN 9 1790356109000", 10_000_000_000L))
        assertEquals("onboarding:ONBOARDING_FAILED", mirror.accept("onboarding: ONBOARDING_FAILED", 10_000_000_000L))
    }

    @Test fun cycleStageBudgetExhaustionIsBoundedAndKeepsTheTerminalSlot() {
        val mirror = NativeStderrMirror(windowNanos = 1_000,
            maxCyclePerWindow = 3, maxCycleTotal = 4, maxVkPerWindow = 1, maxVkTotal = 2)
        var admitted = 0
        repeat(20) { if (mirror.accept("cyclestage: AFTER_ME_READ $it 1790356109000", 0) != null) admitted++ }
        assertEquals(3, admitted) // the cycle per-window bound holds
        // A new window opens bounded cycle slots again, but never past the class total.
        repeat(20) { if (mirror.accept("cyclestage: EMIT_BEGIN $it 1790356109000", 1_000) != null) admitted++ }
        assertEquals(4, admitted)
        assertNull(mirror.accept("cyclestage: GW_REQUEST_BEGIN 9 1790356109000", 2_000))
        // The cycle burst never spends the VK budget or the reserved terminal slot.
        assertEquals("vkstage:CACHE_MISS:0:5:1790356109000:0",
            mirror.accept("vkstage: CACHE_MISS 0 5 1790356109000 0", 2_000))
        assertEquals("onboarding:ONBOARDING_FAILED", mirror.accept("onboarding: ONBOARDING_FAILED", 2_000))
    }

    @Test fun coldVkPreludeKeepsCriticalCycleStages() {
        val mirror = NativeStderrMirror() // production limits: stage 16/64, cycle 16/48, vk 8/24
        val emitted = mutableListOf<String>()
        fun feed(line: String) { mirror.accept(line, 0)?.let { emitted += it } }

        // One realistic cold prelude in a single window: an onboarding stage, an
        // accountaccess code, the whole shared stage budget and the whole VK window budget.
        feed("onboarding: ENTRY")
        feed("accountaccess: TRANSPORT")
        repeat(8) { feed("acctstage: SIGN_BEGIN ${100 + it}") }
        repeat(8) { feed("svcstage: ESTABLISH_BEGIN") }
        val vkPrelude = listOf(
            "vkstage: CACHE_MISS 0 9000 1790356109000 0",
            "vkstage: SERIAL_LOCK_WAIT 0 9000 1790356109000 4500",
            "vkstage: THROTTLE_WAIT 0 9000 1790356109000 2500",
            "vkstage: FETCH_BEGIN 0 9000 1790356109000 0",
            "vkstage: FETCH_END_OK 0 9000 1790356109000 0",
            "vkstage: PROVIDER_LEGACY 0 9000 1790356109000 0",
            "vkstage: HASH_BEGIN 1 9000 1790356109000 0",
            "vkstage: HASH_END 1 9000 1790356109000 0",
        )
        vkPrelude.forEach { feed(it) }

        // The critical post-/me chronology must survive that prelude under default bounds.
        val critical = listOf(
            "cyclestage: ME_RESPONSE_2XX 9000 1790356109000",
            "cyclestage: AFTER_ME_READ 9001 1790356109000",
            "cyclestage: EMIT_BEGIN 9002 1790356109000",
            "cyclestage: EMIT_END_OK 9003 1790356109000",
            "cyclestage: GW_REFRESH_BEGIN 9004 1790356109000",
            "cyclestage: GW_REQUEST_BEGIN 9005 1790356109000",
            "cyclestage: GW_REQUEST_END_4XX 9006 1790356109000",
            // Second cycle after the pending writer: same window, still inside the cycle budget.
            "cyclestage: GW_PENDING_REFRESH_BEGIN 9007 1790356109000",
            "cyclestage: GW_REQUEST_BEGIN 9008 1790356109000",
            "cyclestage: GW_REQUEST_END_2XX 9009 1790356109000",
        )
        critical.forEach { line ->
            val mirrored = mirror.accept(line, 0)
            assertNotNull("critical marker dropped: $line", mirrored)
            emitted += mirrored!!
        }
        val cycleLines = emitted.filter { it.startsWith("cyclestage:") }
        assertEquals(10, cycleLines.size)
        assertEquals("cyclestage:ME_RESPONSE_2XX:9000:1790356109000", cycleLines.first())
        assertEquals("cyclestage:GW_REQUEST_END_2XX:9009:1790356109000", cycleLines.last())

        // The terminal outcome still owns the reserved main slot after the prelude.
        assertEquals("onboarding:ONBOARDING_TIMEOUT",
            mirror.accept("onboarding: ONBOARDING_TIMEOUT", 0))

        // The whole VK window budget was admitted; detail round-trips for the waits while
        // elapsed stays the per-attempt value shared by every vkstage line.
        val vkLines = emitted.filter { it.startsWith("vkstage:") }
        assertEquals(8, vkLines.size)
        assertEquals("vkstage:CACHE_MISS:0:9000:1790356109000:0", vkLines[0])
        assertEquals("vkstage:SERIAL_LOCK_WAIT:0:9000:1790356109000:4500", vkLines[1])
        assertEquals("vkstage:THROTTLE_WAIT:0:9000:1790356109000:2500", vkLines[2])
        assertEquals("vkstage:PROVIDER_LEGACY:0:9000:1790356109000:0", vkLines[5])
    }

    @Test fun drainMirrorsCycleAndVkStageLinesOnly() {
        val stream = ("cyclestage: AFTER_ME_READ 0 1790356109000\n" +
            "vkstage: CACHE_HIT 0 5 1790356109000 0\n" +
            "cyclestage: NOT_A_STAGE 0 1790356109000\n" +
            "vkstage: HASH_BEGIN 1 5 1790356109000 0 token=sekrit\n").byteInputStream()
        val emitted = mutableListOf<String>()
        drainNativeStderr(stream) { emitted += it }
        assertEquals(listOf("cyclestage:AFTER_ME_READ:0:1790356109000",
            "vkstage:CACHE_HIT:0:5:1790356109000:0"), emitted)
    }

    @Test fun serviceErrorCodesAreBoundedAndUnknownCollapsesToOther() {
        val mirror = NativeStderrMirror()
        assertEquals("svcstage:SERVICE_ERROR_SERVICE_BAD_PATH",
            mirror.accept("svcstage: SERVICE_ERROR_SERVICE_BAD_PATH", 0))
        assertEquals("svcstage:SERVICE_ERROR_SERVICE_PATH_DENIED",
            mirror.accept("svcstage: SERVICE_ERROR_SERVICE_PATH_DENIED", 0))
        assertEquals("svcstage:SERVICE_ERROR_SERVICE_BUSY",
            mirror.accept("svcstage: SERVICE_ERROR_SERVICE_BUSY", 0))
        assertEquals("svcstage:SERVICE_ERROR_SERVICE_UNAVAILABLE",
            mirror.accept("svcstage: SERVICE_ERROR_SERVICE_UNAVAILABLE", 0))
        assertEquals("svcstage:SERVICE_ERROR_SERVICE_SEED_BINDING",
            mirror.accept("svcstage: SERVICE_ERROR_SERVICE_SEED_BINDING", 0))
        assertEquals("svcstage:SERVICE_ERROR_OTHER",
            mirror.accept("svcstage: SERVICE_ERROR_OTHER", 0))
        assertEquals("svcstage:RUN_EXIT_CANCELED",
            mirror.accept("svcstage: RUN_EXIT_CANCELED", 0))
        assertEquals("svcstage:RUN_STOP_SOURCE_STDIN_EOF",
            mirror.accept("svcstage: RUN_STOP_SOURCE_STDIN_EOF", 0))
        assertEquals("svcstage:RUN_STOP_SOURCE_STDIN_ERROR",
            mirror.accept("svcstage: RUN_STOP_SOURCE_STDIN_ERROR", 0))
        assertEquals("svcstage:RUN_STOP_SOURCE_CTX_ALREADY_CANCELED",
            mirror.accept("svcstage: RUN_STOP_SOURCE_CTX_ALREADY_CANCELED", 0))
        assertEquals("svcstage:RUN_STOP_SOURCE_EXPLICIT_CANCEL",
            mirror.accept("svcstage: RUN_STOP_SOURCE_EXPLICIT_CANCEL", 0))
        assertEquals("svcstage:RUN_STOP_SOURCE_SIGNAL_TERM",
            mirror.accept("svcstage: RUN_STOP_SOURCE_SIGNAL_TERM", 0))
        assertNull(mirror.accept("svcstage: SERVICE_ERROR_RAW_UNKNOWN", 0))
    }

    @Test fun serviceStagesAreFixedAndUseBoundedStageBudget() {
        val mirror = NativeStderrMirror(maxStagePerWindow = 2, maxStageTotal = 2)
        assertEquals("svcstage:ESTABLISH_BEGIN", mirror.accept("svcstage: ESTABLISH_BEGIN", 0))
        assertEquals("svcstage:ESTABLISH_FAILED", mirror.accept("svcstage: ESTABLISH_FAILED", 0))
        assertNull(mirror.accept("svcstage: FRAME_WRITE_BEGIN", 0))
        assertNull(NativeStderrCodes.code("svcstage: OTHER"))
        assertNull(NativeStderrCodes.code("svcstage: ESTABLISH_BEGIN peer=secret"))
        assertNull(NativeStderrCodes.code("svcstage: ESTABLISH_BEGIN 123"))
        assertEquals("onboarding:ONBOARDING_FAILED", mirror.accept("onboarding: ONBOARDING_FAILED", 0))
    }

    @Test fun serviceLocalRejectReasonsStayFixedTokens() {
        val mirror = NativeStderrMirror()
        listOf("REJECT_METHOD", "REJECT_ORIGIN", "REJECT_PATH", "REJECT_HEADERS",
            "REJECT_BODY", "REJECT_SEED", "REJECT_FRAME").forEach {
            assertEquals("svcstage:$it", mirror.accept("svcstage: $it", 0))
        }
        assertNull(NativeStderrCodes.code("svcstage: REJECT_"))
        assertNull(NativeStderrCodes.code("svcstage: REJECT_UNKNOWN"))
        assertNull(NativeStderrCodes.code("svcstage: REJECT_PATH secret=https://x"))
    }

    @Test fun establishProgressAndPendingMarkerSurviveNormalStartupBudget() {
        val mirror = NativeStderrMirror()
        listOf("ESTABLISH_BEGIN", "ESTABLISH_SEED_READY", "ESTABLISH_VK_BEGIN",
            "ESTABLISH_VK_READY", "ESTABLISH_DIAL_BEGIN", "ESTABLISH_TIMEOUT_UNPROCESSED").forEach {
            assertEquals("svcstage:$it", mirror.accept("svcstage: $it", 0))
        }
        assertNull(NativeStderrCodes.code("svcstage: ESTABLISH_TIMEOUT_UNPROCESSED token=sekrit"))
        assertNull(NativeStderrCodes.code("svcstage: ESTABLISH_UNKNOWN"))
    }

    @Test fun acctStageChronologySurvivesTheMirrorBudgetInOrder() {
        val mirror = NativeStderrMirror()
        val stages = listOf("AUTH_BEGIN", "SERVICE_RESPONSE_RECEIVED", "CHALLENGE_DECODED",
            "SIGN_BEGIN", "SIGN_END", "SESSION_POST_BEGIN", "SESSION_RESPONSE")
        val emitted = mutableListOf<String>()
        stages.forEach { mirror.accept("acctstage: $it 5", 0)?.let { line -> emitted += line } }
        assertEquals(stages.map { "acctstage:$it:5" }, emitted)
        // The seven non-terminal stage lines leave the reserved slot for a terminal outcome.
        assertEquals("onboarding:ONBOARDING_FAILED", mirror.accept("onboarding: ONBOARDING_FAILED", 0))
    }

    @Test fun stageBudgetKeepsTwoRoundsWithoutEvictingOtherSourcesOrTheTerminalSlot() {
        val mirror = NativeStderrMirror() // accepted limits: main 8/10 s/64, stage budget separate
        val emitted = mutableListOf<String>()
        // A preferred 403 round plus its fallback: 1 AUTH_BEGIN + 2x6 session markers = 13 lines.
        val round = listOf("SERVICE_RESPONSE_RECEIVED", "CHALLENGE_DECODED", "SIGN_BEGIN",
            "SIGN_END", "SESSION_POST_BEGIN", "SESSION_RESPONSE")
        (listOf("AUTH_BEGIN") + round + round).forEach {
            mirror.accept("acctstage: $it 7", 0)?.let { line -> emitted += line }
        }
        // Existing accountaccess/onboarding non-terminal lines keep their own reserved-slot budget.
        assertEquals("ACCESS_DENIED", mirror.accept("accountaccess: ACCESS_DENIED", 0))
        repeat(6) { assertEquals("onboarding:ENTRY", mirror.accept("onboarding: ENTRY", 0)) }
        // The seven main-budget slots now stop one short for the terminal, and all 13 stages stayed.
        assertNull(mirror.accept("onboarding: ENTRY", 0))
        assertEquals("onboarding:ONBOARDING_FAILED", mirror.accept("onboarding: ONBOARDING_FAILED", 0))
        assertEquals(13, emitted.size)
        assertEquals("acctstage:AUTH_BEGIN:7", emitted.first())
        assertEquals("acctstage:SESSION_RESPONSE:7", emitted.last())
        // A still-unknown transport completion is a distinct fixed stage, not a received response.
        assertEquals("acctstage:SESSION_POST_END:7", NativeStderrCodes.code("acctstage: SESSION_POST_END 7"))
    }

    @Test fun stageBudgetIsBoundedPerWindowAndTotal() {
        val mirror = NativeStderrMirror(maxPerWindow = 2, windowNanos = 1_000, maxTotal = 3,
            maxStagePerWindow = 4, maxStageTotal = 6)
        var admitted = 0
        repeat(20) { if (mirror.accept("acctstage: SIGN_BEGIN $it", 0) != null) admitted++ }
        assertEquals(4, admitted) // the dedicated per-window bound holds
        // A new window opens bounded stage slots again, but never past the dedicated total.
        repeat(20) { if (mirror.accept("acctstage: SIGN_END $it", 1_000) != null) admitted++ }
        assertEquals(6, admitted)
        assertNull(mirror.accept("acctstage: SIGN_BEGIN 9", 2_000))
        // The stage budget never consumes the reserved terminal slot of the main budget.
        assertEquals("onboarding:ONBOARDING_FAILED", mirror.accept("onboarding: ONBOARDING_FAILED", 2_000))
    }

    @Test fun acceptsSecretFreeSvcTraceCorrelation() {
        assertEquals("svctrace:gen=3:class=PLANS:event=ESTABLISH_FAIL:reused=0:port=0",
            NativeStderrCodes.code("svctrace: gen=3 class=PLANS event=ESTABLISH_FAIL reused=0 port=0"))
        assertEquals("plansdiag:BEGIN", NativeStderrCodes.code("plansdiag: BEGIN"))
        assertEquals("plansdiag:TRANSPORT", NativeStderrCodes.code("plansdiag: TRANSPORT"))
        assertNull(NativeStderrCodes.code("plansdiag: TRANSPORT token=sekrit"))
        assertEquals("svctrace:gen=3:class=GATEWAYS:event=WRITE_OK:reused=0:port=51409",
            NativeStderrCodes.code("svctrace: gen=3 class=GATEWAYS event=WRITE_OK reused=0 port=51409"))
        assertEquals("svctrace:gen=1:class=AUTH:event=READ_FAIL:reused=1:port=0",
            NativeStderrCodes.code("svctrace: gen=1 class=AUTH event=READ_FAIL reused=1 port=0"))
        // Unknown class/event, bad reused flag and out-of-range port are rejected.
        assertEquals("svctrace:gen=2:class=USAGE:event=WRITE_OK:reused=1:port=0",
            NativeStderrCodes.code("svctrace: gen=2 class=USAGE event=WRITE_OK reused=1 port=0"))
        assertEquals("svctrace:gen=2:class=USAGE:event=READ_OK:reused=1:port=0",
            NativeStderrCodes.code("svctrace: gen=2 class=USAGE event=READ_OK reused=1 port=0"))
        assertNull(NativeStderrCodes.code("svctrace: gen=1 class=SECRET event=READ_OK reused=0 port=1"))
        assertNull(NativeStderrCodes.code("svctrace: gen=1 class=USAGE2 event=READ_OK reused=0 port=1"))
        assertNull(NativeStderrCodes.code("svctrace: gen=1 class=ME event=OTHER reused=0 port=1"))
        assertNull(NativeStderrCodes.code("svctrace: gen=1 class=ME event=READ_OK reused=2 port=1"))
        assertNull(NativeStderrCodes.code("svctrace: gen=1 class=ME event=READ_OK reused=0 port=70000"))
        assertNull(NativeStderrCodes.code("svctrace: gen=1 class=ME event=READ_OK reused=0 port=1 token=sekrit"))
        assertNull(NativeStderrCodes.code("svctrace: gen=1 class=PLANS event=READ_OK reused=0 port=1 token=sekrit"))
        // Extended bounded fields of the service-exchange correlation.
        assertEquals(
            "svctrace:gen=2:class=GATEWAYS:event=READ_FAIL:reused=1:port=51409:xid=17:elapsed_ms=88:req=3:idle_ms=1250:err=EOF",
            NativeStderrCodes.code("svctrace: gen=2 class=GATEWAYS event=READ_FAIL reused=1 port=51409 xid=17 elapsed_ms=88 req=3 idle_ms=1250 err=EOF"))
        assertEquals(
            "svctrace:gen=1:class=ME:event=EXCHANGE_END:reused=0:port=0:xid=1:elapsed_ms=15000:err=TIMEOUT",
            NativeStderrCodes.code("svctrace: gen=1 class=ME event=EXCHANGE_END reused=0 port=0 xid=1 elapsed_ms=15000 err=TIMEOUT"))
        assertEquals(
            "svctrace:gen=1:class=AUTH:event=REUSE_STALE_IDLE:reused=1:port=41000:xid=2:idle_ms=8001",
            NativeStderrCodes.code("svctrace: gen=1 class=AUTH event=REUSE_STALE_IDLE reused=1 port=41000 xid=2 idle_ms=8001"))
        // Unknown error class or malformed field order is rejected.
        assertNull(NativeStderrCodes.code("svctrace: gen=1 class=ME event=READ_FAIL reused=0 port=1 xid=1 elapsed_ms=5 err=SEKRIT"))
        assertNull(NativeStderrCodes.code("svctrace: gen=1 class=ME event=READ_FAIL reused=0 port=1 elapsed_ms=5 xid=1"))
        assertNull(NativeStderrCodes.code("svctrace: gen=1 class=ME event=READ_FAIL reused=0 port=1 xid=1 elapsed_ms=5 err=NONE"))
    }

    @Test fun acceptsRunnerPendingAndRetryMarkerTokens() {
        for (token in listOf("GW_PENDING_REFRESH_END", "ATTEMPT_RETRY_SLEEP", "ATTEMPT_RETRY_WAIT", "ATTEMPT_TERMINAL")) {
            assertEquals("cyclestage:$token:12:1790796172000",
                NativeStderrCodes.code("cyclestage: $token 12 1790796172000"))
        }
    }

    @Test fun svcTraceBudgetIsDedicatedAndBounded() {
        val mirror = NativeStderrMirror() // production limits
        var admitted = 0
        repeat(200) { i -> if (mirror.accept("svctrace: gen=1 class=ME event=READ_OK reused=0 port=$i", 0) != null) admitted++ }
        // Dedicated budget is separate from the small main budget, so a stage burst cannot evict it.
        assertTrue(admitted > 7)
        // Other streams still keep their reserved terminal slot.
        assertEquals("onboarding:ONBOARDING_FAILED", mirror.accept("onboarding: ONBOARDING_FAILED", 0))
    }

    @Test fun rejectsUnboundedOrPayloadBearingLines() {
        assertNull(NativeStderrCodes.code("accountaccess: " + "A".repeat(65)))
        assertNull(NativeStderrCodes.code("accountaccess: SUBSCRIPTION_MISSING token=sekrit key=..."))
        assertNull(NativeStderrCodes.code("{\"token\":\"sekrit\"}"))
        assertNull(NativeStderrCodes.code(""))
    }

    @Test fun nonTerminalLinesReserveTheLastWindowAndTotalSlot() {
        val mirror = NativeStderrMirror(maxPerWindow = 2, windowNanos = 1_000, maxTotal = 3)
        assertEquals("ACCESS_DENIED", mirror.accept("accountaccess: ACCESS_DENIED", 0))
        // A non-terminal line leaves the last slot of the window for a terminal.
        assertNull(mirror.accept("accountaccess: ACCESS_DENIED", 100))
        assertEquals("onboarding:ONBOARDING_FAILED", mirror.accept("onboarding: ONBOARDING_FAILED", 100))
        // The hard per-window size still holds for terminals.
        assertNull(mirror.accept("onboarding: ONBOARDING_FAILED", 200))
        // A new window cannot bypass the total reservation for non-terminal lines.
        assertNull(mirror.accept("accountaccess: ACCESS_DENIED", 1_000))
        // The terminal may still spend the last global slot; then the hard total cap holds.
        assertEquals("onboarding:ONBOARDING_FAILED", mirror.accept("onboarding: ONBOARDING_FAILED", 1_000))
        assertNull(mirror.accept("onboarding: ONBOARDING_FAILED", 2_000))
    }

    @Test fun mixedBackgroundAndStageBurstCannotEvictTheTerminalLine() {
        val mirror = NativeStderrMirror() // accepted production limits: 8 lines / 10 s / 64 total
        val emitted = mutableListOf<String>()
        mirror.accept("accountaccess: TRANSPORT", 0)?.let { emitted += it }
        repeat(7) { mirror.accept("onboarding: ENTRY", 0)?.let { emitted += it } }
        mirror.accept("onboarding: ONBOARDING_TIMEOUT", 0)?.let { emitted += it }
        // One accountaccess line plus six admitted stages leave the eighth slot for the terminal.
        assertEquals(listOf("TRANSPORT", "onboarding:ENTRY", "onboarding:ENTRY", "onboarding:ENTRY",
            "onboarding:ENTRY", "onboarding:ENTRY", "onboarding:ENTRY", "onboarding:ONBOARDING_TIMEOUT"),
            emitted)
        assertEquals(8, emitted.size) // the hard 8/10 s window was neither exceeded nor raised
        // Nothing else can enter the full window, terminal included.
        assertNull(mirror.accept("onboarding: ONBOARDING_TIMEOUT", 0))
        assertNull(mirror.accept("accountaccess: TRANSPORT", 0))
    }

    @Test fun backgroundBurstCannotEvictTheTerminalLine() {
        val mirror = NativeStderrMirror()
        var backgroundLines = 0
        repeat(200) { if (mirror.accept("accountaccess: RATE_LIMITED", 0) != null) backgroundLines++ }
        assertEquals(7, backgroundLines) // the non-terminal budget stops one line short
        assertEquals("onboarding:ONBOARDING_FAILED",
            mirror.accept("onboarding: ONBOARDING_FAILED", 0))
        assertNull(mirror.accept("accountaccess: RATE_LIMITED", 0))
        assertNull(mirror.accept("onboarding: ONBOARDING_FAILED", 0))
    }

    @Test fun onboardingStageBurstCannotEvictTheTerminalLine() {
        val mirror = NativeStderrMirror() // accepted production limits: 8 lines / 10 s / 64 total
        var stageLines = 0
        repeat(200) { if (mirror.accept("onboarding: ENTRY", 0) != null) stageLines++ }
        // The stage burst spends at most maxPerWindow - 1 lines of the current window.
        assertEquals(7, stageLines)
        assertEquals("onboarding:ONBOARDING_PENDING_BUDGET",
            mirror.accept("onboarding: ONBOARDING_PENDING_BUDGET", 0))
        // The window is now at its accepted size; the next window opens for a new run.
        assertNull(mirror.accept("onboarding: ONBOARDING_PENDING_BUDGET", 1))
        assertEquals("onboarding:ENTRY", mirror.accept("onboarding: ENTRY", 10_000_000_000L))
    }

    @Test fun onboardingStageBurstCannotExhaustTheTotalBudgetBeforeATerminal() {
        val mirror = NativeStderrMirror(maxPerWindow = 100, windowNanos = 1_000, maxTotal = 4)
        var stageLines = 0
        repeat(10) { index -> if (mirror.accept("onboarding: ENTRY", index * 1_000L) != null) stageLines++ }
        assertEquals(3, stageLines) // one of the four total slots stays reserved
        assertEquals("onboarding:ONBOARDING_FAILED", mirror.accept("onboarding: ONBOARDING_FAILED", 20_000))
        assertNull(mirror.accept("onboarding: ONBOARDING_FAILED", 21_000))
    }

    @Test fun previousTerminalsAndStagesRespectTheHonestTotalBound() {
        val mirror = NativeStderrMirror(maxPerWindow = 8, windowNanos = 1_000, maxTotal = 4)
        // Three earlier terminals spend the total budget down to the reserved final slot.
        assertEquals("onboarding:ONBOARDING_FAILED", mirror.accept("onboarding: ONBOARDING_FAILED", 0))
        assertEquals("onboarding:ONBOARDING_TIMEOUT", mirror.accept("onboarding: ONBOARDING_TIMEOUT", 0))
        assertEquals("onboarding:ONBOARDING_CANCELLED", mirror.accept("onboarding: ONBOARDING_CANCELLED", 0))
        // A fresh window's stage/background line cannot spend that last slot...
        assertNull(mirror.accept("onboarding: ENTRY", 1_000))
        assertNull(mirror.accept("accountaccess: TRANSPORT", 1_000))
        // ...the next terminal does, and then the hard total cap is honest for everything.
        assertEquals("onboarding:STARTED", mirror.accept("onboarding: STARTED", 1_000))
        assertNull(mirror.accept("onboarding: STARTED", 2_000))
        assertNull(mirror.accept("onboarding: ENTRY", 2_000))
        assertNull(mirror.accept("accountaccess: TRANSPORT", 2_000))
    }

    @Test fun pendingRetryRepeatsKeepEveryTerminalWhileBudgetLasts() {
        val mirror = NativeStderrMirror() // same accepted limits; nothing raised
        var emittedStages = 0
        var emittedTerminals = 0
        var nowNanos = 0L
        repeat(40) { // explicit connect runs: one ENTRY stage plus one terminal pending outcome
            if (mirror.accept("onboarding: ENTRY", nowNanos) != null) emittedStages++
            if (mirror.accept("onboarding: ONBOARDING_PENDING_BUDGET", nowNanos) != null) emittedTerminals++
            nowNanos += 10_000_000_000L
        }
        assertEquals(32, emittedStages)
        assertEquals(32, emittedTerminals)
    }

    @Test fun drainSplitsLinesAndEmitsOnlyCodes() {
        val stream = ("noise line 1\naccountaccess: SUBSCRIPTION_MISSING\n" +
            "accountaccess: ACCESS_DENIED token=sekrit\n" +
            "onboarding: ENTRY\n" +
            "onboarding: HTTP409 ONBOARDING_START_CONFLICT\n" +
            "onboarding: ONBOARDING_UNKNOWN\n" +
            "accountaccess: REJECTED\ntrailing").byteInputStream()
        val emitted = mutableListOf<String>()
        drainNativeStderr(stream) { emitted += it }
        assertEquals(listOf("SUBSCRIPTION_MISSING", "onboarding:ENTRY",
            "onboarding:ONBOARDING_UNKNOWN", "REJECTED"), emitted)
    }

    @Test fun overlongAndNoisyLinesAreDiscardedWithoutParsing() {
        val emitted = mutableListOf<String>()
        drainNativeStderr(("x".repeat(9_000) + "\n" + "y".repeat(500) + "\n").byteInputStream()) { emitted += it }
        assertTrue(emitted.isEmpty())
    }

    @Test
    fun usageStageBudgetCannotEvictRuntimeCancelProducerMarker() {
        // Deliberately small GENERAL cap that the old shared-budget code would saturate:
        // non-terminal general lines are capped at maxTotal-1 / maxPerWindow-1. Flood the
        // usage stream past its own dedicated cap; the producer markers must still be admitted.
        val mirror = NativeStderrMirror(
            maxPerWindow = 8, maxTotal = 8,
            maxUsagePerWindow = 4, maxUsageTotal = 4,
        )
        for (i in 1..50) mirror.accept("usagestage: FAIL_TRANSPORT", 0L)
        assertNull(mirror.accept("usagestage: FAIL_SERVICE_PATH_DENIED", 0L))
        assertEquals("vpnstage:RUNTIME_CANCEL:HOST_STOP",
            mirror.accept("vpnstage: RUNTIME_CANCEL HOST_STOP", 0L))
        assertEquals("vpnstage:CANCEL_SOURCE:WORKER_TERMINAL",
            mirror.accept("vpnstage: CANCEL_SOURCE WORKER_TERMINAL", 0L))
    }

    @Test
    fun `refreshstage accepts the five fixed manual refresh lines and rejects the rest`() {
        val mirror = NativeStderrMirror()
        assertEquals("refreshstage:receive:1", mirror.accept("refreshstage:receive:1", 0))
        assertEquals("refreshstage:accepted", mirror.accept("refreshstage:accepted", 0))
        assertEquals("refreshstage:coalesced", mirror.accept("refreshstage:coalesced", 0))
        assertEquals("refreshstage:manual_cycle_begin", mirror.accept("refreshstage:manual_cycle_begin", 0))
        assertEquals("refreshstage:manual_cycle_finish", mirror.accept("refreshstage:manual_cycle_finish", 0))
        assertNull(mirror.accept("refreshstage:unknown", 0))
        assertNull(mirror.accept("refreshstage:receive", 0))
        assertNull(mirror.accept("refreshstage:receive:99999999", 0))
        assertNull(mirror.accept("refreshstage:receive:1:extra", 0))
        assertNull(mirror.accept("refreshstage:accepted:123", 0))
        assertNull(mirror.accept("raw stderr payload", 0))
    }

    @Test
    fun `refreshstage pair burst fits its dedicated budget`() {
        val mirror = NativeStderrMirror()
        // A controlled rapid pair: receive, accepted, (duplicate) receive, coalesced,
        // manual_cycle_begin, manual_cycle_finish = 6 lines must all pass in one window.
        val lines = listOf(
            "refreshstage:receive:1", "refreshstage:accepted",
            "refreshstage:receive:2", "refreshstage:coalesced",
            "refreshstage:manual_cycle_begin", "refreshstage:manual_cycle_finish",
        )
        lines.forEachIndexed { index, line ->
            assertEquals(line, mirror.accept(line, index.toLong()))
        }
    }
}

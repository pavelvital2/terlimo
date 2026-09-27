package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import java.io.File
import java.math.BigInteger

class BootstrapDiagnosticsTest {
    private val attempt = "local-test-attempt"
    private fun event() = JSONObject().put("v", 1).put("type", "bootstrap_diagnostic").put("attempt_id", attempt)
        .put("operation", "CATALOG").put("io_stage", "READ").put("handshake_pin_ok", true)
        .put("caller_canceled", false).put("caller_deadline", false)
        .put("transport_canceled", false).put("transport_deadline", false)
        .put("first_close", "NONE").put("error_class", "NONE").put("failed", false).put("elapsed_ms", 25L)
    private fun snapshot(event: JSONObject) = requireNotNull(BootstrapDiagnostics.parse(event, attempt))

    @Test fun exportsOnlyBoundedDiagnosticFieldsAndAcceptsEveryEnum() {
        val snapshot = snapshot(event())
        assertEquals(setOf("operation", "io_stage", "handshake_pin_ok", "caller_canceled", "caller_deadline",
            "transport_canceled", "transport_deadline", "first_close", "error_class", "failed", "elapsed_ms"), snapshot.keys)
        val variants = mapOf(
            "operation" to listOf("UNKNOWN", "CHALLENGE", "CATALOG", "STATUS", "REFRESH", "REGISTER"),
            "io_stage" to listOf("DIAL", "HANDSHAKE", "WRITE", "READ", "COMPLETE"),
            "first_close" to listOf("NONE", "UNKNOWN", "RELAY_READ", "RELAY_WRITE", "PIPE_READ", "PIPE_WRITE", "CALLER_CANCEL", "CLEANUP"),
            "error_class" to listOf("NONE", "EOF", "CANCELED", "DEADLINE", "OTHER"))
        for ((key, values) in variants) {
            for (value in values) assertEquals(value, snapshot(event().put(key, value))[key])
            assertNull(BootstrapDiagnostics.parse(event().put(key, "raw secret address or error"), attempt))
            assertNull(BootstrapDiagnostics.parse(event().put(key, 1), attempt))
        }
    }

    @Test fun rejectsUnknownMissingOrUncorrelatedFields() {
        assertNull(BootstrapDiagnostics.parse(event().put("raw_error", "must never export"), attempt))
        for (key in event().keys().asSequence().toList()) {
            assertNull(BootstrapDiagnostics.parse(event().apply { remove(key) }, attempt))
        }
        assertNull(BootstrapDiagnostics.parse(event().put("v", 2), attempt))
        assertNull(BootstrapDiagnostics.parse(event().put("v", "1"), attempt))
        assertNull(BootstrapDiagnostics.parse(event().put("type", "diagnostic"), attempt))
        assertNull(BootstrapDiagnostics.parse(event(), "stale-attempt"))
    }

    @Test fun rejectsTypeCoercionFractionalOverflowAndNegativeElapsed() {
        for (key in listOf("handshake_pin_ok", "caller_canceled", "caller_deadline", "transport_canceled", "transport_deadline", "failed")) {
            for (value in listOf("true", 1, JSONObject.NULL))
                assertNull(BootstrapDiagnostics.parse(event().put(key, value), attempt))
            assertEquals(true, snapshot(event().put(key, true))[key])
        }
        for (value in listOf(-1L, "25", 25.0, 0.5, BigInteger("9223372036854775808"), JSONObject.NULL))
            assertNull(BootstrapDiagnostics.parse(event().put("elapsed_ms", value), attempt))
        assertEquals(0L, snapshot(event().put("elapsed_ms", 0))["elapsed_ms"])
        assertEquals(Long.MAX_VALUE, snapshot(event().put("elapsed_ms", Long.MAX_VALUE))["elapsed_ms"])
    }

    @Test fun successUpdatesLatestButFirstFailureSurvivesTransportCancelCallerCancelAndCleanup() {
        val success = snapshot(event())
        val laterSuccess = snapshot(event().put("operation", "REFRESH"))
        var latest = BootstrapDiagnostics.retainFirstFailure(emptyMap(), success)
        latest = BootstrapDiagnostics.retainFirstFailure(latest, laterSuccess)
        assertSame(laterSuccess, latest)
        val remoteEof = snapshot(event().put("failed", true).put("error_class", "EOF").put("first_close", "RELAY_READ"))
        latest = BootstrapDiagnostics.retainFirstFailure(latest, remoteEof)
        val transportCancel = snapshot(event().put("failed", true).put("error_class", "CANCELED")
            .put("transport_canceled", true).put("first_close", "PIPE_READ"))
        val callerCancel = snapshot(event().put("failed", true).put("error_class", "CANCELED")
            .put("caller_canceled", true).put("first_close", "CALLER_CANCEL"))
        val cleanup = snapshot(event().put("failed", true).put("first_close", "CLEANUP").put("transport_canceled", true))
        for (next in listOf(transportCancel, callerCancel, cleanup, success))
            latest = BootstrapDiagnostics.retainFirstFailure(latest, next)
        assertSame(remoteEof, latest)
        assertEquals(false, latest["transport_canceled"])
        assertEquals(false, latest["caller_canceled"])
        for (first in listOf(transportCancel, callerCancel, cleanup)) {
            val retained = BootstrapDiagnostics.retainFirstFailure(first, remoteEof)
            assertSame(first, retained)
        }
        // A new attempt starts empty and can observe a different first failure.
        assertSame(callerCancel, BootstrapDiagnostics.retainFirstFailure(emptyMap(), callerCancel))
    }

    @Test fun androidDispatchSavesBeforeTerminalBypassWithoutTeardownActions() {
        val suffix = "src/main/java/xyz/terlimo/test/SessionService.kt"
        val source = listOf(File(suffix), File("testapp/$suffix")).first { it.isFile }.readText()
        val dispatch = source.substringAfter("private fun receiveBridge(").substringBefore("private fun send(")
        val diagnostic = dispatch.substringAfter("if (type == \"bootstrap_diagnostic\") {")
            .substringBefore("// Terminal notifications")
        assertTrue(diagnostic.contains("BootstrapDiagnostics.parse(event, attempt)"))
        assertTrue(diagnostic.contains("gate.ifActive(attempt)"))
        assertTrue(diagnostic.contains("BootstrapDiagnostics.retainFirstFailure(lastBootstrap, snapshot)"))
        assertTrue(diagnostic.contains("return"))
        assertFalse(diagnostic.contains("stopAttempt("))
        assertFalse(diagnostic.contains("actor.submit"))
        assertTrue(dispatch.indexOf("BootstrapDiagnostics.parse") < dispatch.indexOf("type == \"error\""))
        assertTrue(dispatch.indexOf("type == \"error\"") < dispatch.indexOf("actor.submit"))
        val stop = source.substringAfter("private fun stopAttempt(").substringBefore("override fun onDestroy()")
        // Teardown may read the frozen facts for the display-only completed
        // snapshot, but must not clear/replace the first bootstrap failure.
        assertFalse(Regex("lastBootstrap\\s*=").containsMatchIn(stop))
        assertTrue(stop.contains("completedDiagnostics = UserDiagnostics.describe(lastBootstrap, lastReadiness, lastRelay, lastVpnDiagnostic)"))
        assertFalse(stop.contains("lastPhysicalNetwork"))
        assertTrue(source.substringAfter("private fun begin(").substringBefore("private fun handle(").contains("lastBootstrap = emptyMap()"))
    }
}

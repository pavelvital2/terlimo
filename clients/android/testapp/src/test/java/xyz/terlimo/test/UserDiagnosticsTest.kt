package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test
import java.io.File

class UserDiagnosticsTest {
    @Test fun configWorkerStartsAreShownOnlyWhenObserved() {
        assertTrue(UserDiagnostics.describe(emptyMap(), emptyMap(), mapOf("config_worker_start_count" to 11L)).contains("config_worker_start_count: 11"))
        assertFalse(UserDiagnostics.describe(emptyMap(), emptyMap(), emptyMap()).contains("config_worker_start_count:"))
        assertFalse(UserDiagnostics.describe(emptyMap(), emptyMap(), mapOf("config_worker_start_count" to -1L)).contains("config_worker_start_count:"))
    }
    @Test fun allowsOnlyFixedTypedFactsAndNoRawFields() {
        val text = UserDiagnostics.describe(mapOf("operation" to "STATUS", "error_class" to "EOF", "failed" to true,
            "io_stage" to "secret-stage", "token" to "private-token"),
            mapOf("dnsOk" to false, "httpsOk" to "private-message", "raw_error" to "private-error"),
            mapOf("local_udp_write_errors" to 0L, "private-id" to 100L, "relay_rx_packets" to -1L))
        assertTrue(text.contains("operation: STATUS")); assertTrue(text.contains("dnsOk: false"))
        assertFalse(text.contains("private")); assertFalse(text.contains("secret-stage"))
        assertFalse(text.contains("relay_rx_packets")); assertFalse(text.contains("httpsOk:"))
        assertTrue(text.contains("не устанавливает причину"))
    }

    @Test fun uplinkCountersAreShownOnlyWhenObservedAndNeverRaw() {
        val text = UserDiagnostics.describe(emptyMap(), emptyMap(),
            mapOf("local_udp_rx_packets" to 7L, "local_udp_rx_bytes" to 1234L,
                "local_udp_write_packets" to 0L, "private-id" to 100L, "local_udp_rx_bytes_extra" to 5L))
        assertTrue(text.contains("local_udp_rx_packets: 7"))
        assertTrue(text.contains("local_udp_rx_bytes: 1234"))
        assertTrue(text.contains("local_udp_write_packets: 0"))
        assertFalse(text.contains("private"))
        assertFalse(text.contains("local_udp_rx_bytes_extra"))
        assertFalse(UserDiagnostics.describe(emptyMap(), emptyMap(), mapOf("local_udp_rx_packets" to -1L))
            .contains("local_udp_rx_packets:"))
        assertFalse(UserDiagnostics.describe(emptyMap(), emptyMap(), emptyMap()).contains("local_udp_rx"))
    }

    @Test fun readinessSubCauseIsShownWithFixedEnumsAndNoRawValues() {
        val text = UserDiagnostics.describe(emptyMap(), mapOf(
            "stage" to "HTTPS", "exceptionClass" to "TLS", "failureCode" to "HTTPS_FAILED",
            "elapsed_ms" to 5_000L, "httpsOk" to false), emptyMap())
        assertTrue(text.contains("stage: HTTPS"))
        assertTrue(text.contains("exceptionClass: TLS"))
        assertTrue(text.contains("failureCode: HTTPS_FAILED"))
        assertTrue(text.contains("readiness_elapsed_ms: 5000"))
        assertTrue(text.contains("httpsOk: false"))
    }

    @Test fun unknownReadinessSubCauseValuesAreNeverEchoed() {
        val text = UserDiagnostics.describe(emptyMap(), mapOf(
            "stage" to "private-stage", "exceptionClass" to "private-exc",
            "failureCode" to "private-code", "elapsed_ms" to -1L), emptyMap())
        assertFalse(text.contains("private"))
        assertFalse(text.contains("stage:"))
        assertFalse(text.contains("exceptionClass:"))
        assertFalse(text.contains("failureCode:"))
    }

    @Test fun readinessFailureSiteLogsOnlyBoundedSubCauseWithoutChangingReadiness() {
        val source = listOf(
            File("src/main/java/xyz/terlimo/test/SessionService.kt"),
            File("testapp/src/main/java/xyz/terlimo/test/SessionService.kt")
        ).first { it.isFile }.readText()
        assertTrue(source.contains("android.util.Log.w(\"WDTT/Readiness\""))
        assertTrue(source.contains("ReadinessDiagnostics.line("))
        assertFalse(source.contains("WDTT/Readiness\", FailureDiagnostics.line("))
        val idx = source.indexOf("failedCode = if (probe.expired && evidence.ready)")
        assertTrue(idx >= 0)
        val after = source.substring(idx).substringBefore("check(evidence.ready")
        assertTrue(after.contains("if (!evidence.ready)"))
        assertTrue(after.contains("evidence.stage.name"))
        assertTrue(after.contains("evidence.exceptionClass.name"))
        assertTrue(after.contains("evidence.dnsOk"))
        assertTrue(after.contains("evidence.httpsOk"))
        assertTrue(after.contains("evidence.httpStatusOk"))
        assertTrue(after.contains("evidence.expectedExitOk"))
        assertTrue(after.contains("probe.elapsedMs"))
        assertTrue(after.contains("probe.expired"))
        assertTrue(after.contains("evidence.failureCode()"))
    }

    @Test fun unavailableIsNotClaimedToBeAnArchivedResult() {
        val text = UserDiagnostics.describe(emptyMap(), emptyMap(), emptyMap())
        assertTrue(text.contains("Данных ещё нет")); assertTrue(text.contains("не архив"))
    }

    @Test fun uiReadsOnlyCompletedAttemptFrozenBeforeSuccessorCanStart() {
        fun source(name: String): String {
            val suffix = "src/main/java/xyz/terlimo/test/$name.kt"
            return listOf(File(suffix), File("testapp/$suffix")).first { it.isFile }.readText()
        }
        val stop = source("SessionService").substringAfter("private fun stopAttempt(").substringBefore("override fun onDestroy()")
        assertTrue(stop.indexOf("actor.awaitStopped") < stop.indexOf("completedDiagnostics ="))
        assertTrue(stop.indexOf("completedDiagnostics =") < stop.indexOf("retiringActors.decrementAndGet()"))
        val ui = source("MainActivity")
        assertTrue(ui.contains("SessionService.completedDiagnostics"))
        assertFalse(ui.contains("SessionService.lastBootstrap"))
        assertFalse(ui.contains("SessionService.lastReadiness"))
        assertFalse(ui.contains("SessionService.lastRelay"))
    }

    @Test fun terminalVpnEvidenceBypassesActorBeforeTerminalCleanup() {
        val source = listOf(
            File("src/main/java/xyz/terlimo/test/SessionService.kt"),
            File("testapp/src/main/java/xyz/terlimo/test/SessionService.kt")
        ).first { it.isFile }.readText()
        val receive = source.substringAfter("private fun receiveBridge(").substringBefore("private fun send(")
        assertTrue(receive.indexOf("vpn_terminal") < receive.indexOf("type == \"error\""))
        assertTrue(receive.contains("retainManagedDiagnostic(event, true)"))
    }
}

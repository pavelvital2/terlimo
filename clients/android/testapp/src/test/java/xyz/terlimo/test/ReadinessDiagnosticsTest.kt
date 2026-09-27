package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class ReadinessDiagnosticsTest {
    @Test fun printsEveryFrozenFieldInFixedOrder() {
        assertEquals(
            "stage=HTTPS exception=TLS failure=HTTPS_FAILED " +
                "dnsOk=true httpsOk=false httpStatusOk=false expectedExitOk=false elapsed_ms=5000 deadline=false",
            ReadinessDiagnostics.line(
                "HTTPS", "TLS", "HTTPS_FAILED",
                dnsOk = true, httpsOk = false, httpStatusOk = false, expectedExitOk = false,
                elapsedMs = 5_000L, deadline = false
            )
        )
    }

    @Test fun includesDeadlineAndElapsedWhenExpiredOrReadyFactsChange() {
        val text = ReadinessDiagnostics.line(
            "EXPECTED_EXIT", "NONE", "EXPECTED_EXIT_MISMATCH",
            dnsOk = true, httpsOk = true, httpStatusOk = true, expectedExitOk = false,
            elapsedMs = 15_000L, deadline = true
        )
        assertTrue(text.contains("expectedExitOk=false"))
        assertTrue(text.contains("elapsed_ms=15000"))
        assertTrue(text.contains("deadline=true"))
    }

    @Test fun unknownStageExceptionCodeAndElapsedFallBackWithoutEchoingInput() {
        val secret = "synthetic-token-private-key-response-body"
        val text = ReadinessDiagnostics.line(
            secret, secret, secret,
            dnsOk = false, httpsOk = false, httpStatusOk = false, expectedExitOk = false,
            elapsedMs = -1L, deadline = false
        )
        assertEquals(
            "stage=- exception=- failure=UNKNOWN " +
                "dnsOk=false httpsOk=false httpStatusOk=false expectedExitOk=false elapsed_ms=-1 deadline=false",
            text
        )
        assertFalse(text.contains("synthetic"))
        assertFalse(text.contains("private"))
        assertFalse(text.contains("token"))
    }

    @Test fun elapsedOutsideBoundedRangeFallsBackToSentinel() {
        assertTrue(ReadinessDiagnostics.line("DNS", "DNS", "DNS_FAILED", true, true, true, true,
            600_001L, false).contains("elapsed_ms=-1"))
        assertTrue(ReadinessDiagnostics.line("DNS", "DNS", "DNS_FAILED", true, true, true, true,
            0L, false).contains("elapsed_ms=0"))
    }

    @Test fun formatterIsWiredAtTheReadinessFailureSiteWithoutChangingReadiness() {
        val service = listOf(
            java.io.File("src/main/java/xyz/terlimo/test/SessionService.kt"),
            java.io.File("testapp/src/main/java/xyz/terlimo/test/SessionService.kt")
        ).first { it.isFile }.readText()
        assertTrue(service.contains("WDTT/Readiness"))
        assertTrue(service.contains("ReadinessDiagnostics.line("))
        assertFalse(service.contains("WDTT/Readiness\", FailureDiagnostics.line("))
        val idx = service.indexOf("failedCode = if (probe.expired && evidence.ready)")
        assertTrue(idx >= 0)
        val after = service.substring(idx).substringBefore("check(evidence.ready")
        assertTrue(after.contains("probe.elapsedMs"))
        assertTrue(after.contains("probe.expired"))
        assertTrue(after.contains("evidence.failureCode()"))
    }
}

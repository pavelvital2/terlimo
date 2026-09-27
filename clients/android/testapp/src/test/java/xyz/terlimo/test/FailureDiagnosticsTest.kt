package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class FailureDiagnosticsTest {
    private val attempt = "123e4567-e89b-12d3-a456-426614174000"

    @Test fun recordsBoundaryPhaseAttemptSafeCodeAndExceptionClass() {
        assertEquals(
            "boundary=stop phase=BootstrapConnecting attempt=$attempt code=NATIVE_EXIT exception=-",
            FailureDiagnostics.line("stop", "BootstrapConnecting", attempt, "NATIVE_EXIT", null)
        )
        assertEquals(
            "boundary=begin phase=ImportVerified attempt=$attempt code=NATIVE_UNAVAILABLE exception=IllegalStateException",
            FailureDiagnostics.line("begin", "ImportVerified", attempt, "NATIVE_UNAVAILABLE", "IllegalStateException")
        )
    }

    /** The UI collapses these unknown native terminals to HOST_ERROR; the diagnostic keeps the code. */
    @Test fun nativeTerminalCodesHiddenByUiAreStillRecorded() {
        for (code in listOf("NATIVE_EXIT", "BRIDGE_INVALID", "BRIDGE_TOO_LARGE", "BRIDGE_CORRELATION", "BRIDGE_EMPTY")) {
            assertTrue(
                "expected UI to collapse $code",
                UserStatusText.error(code).orEmpty().startsWith("HOST_ERROR\n")
            )
            assertTrue(
                "expected diagnostic to keep $code",
                FailureDiagnostics.line("stop", "BootstrapConnecting", attempt, code, null).contains("code=$code")
            )
        }
    }

    @Test fun arbitrarySecretLikeValuesAreNeverEchoed() {
        val link = "whitelists://AAAABBBBCCCCDDDD?token=super-secret"
        val token = "super-secret"
        val line = FailureDiagnostics.line(link, token, link, link, token)
        assertEquals("boundary=- phase=- attempt=- code=HOST_ERROR exception=-", line)
        assertFalse(line.contains("whitelists"))
        assertFalse(line.contains("super-secret"))
        assertFalse(line.contains("token"))
    }

    @Test fun nullsAreExplicitAndOutputIsBounded() {
        assertEquals("boundary=- phase=- attempt=- code=- exception=-",
            FailureDiagnostics.line("", "", null, null, null))
        val long = "A".repeat(500)
        val line = FailureDiagnostics.line("stop", "BootstrapConnecting", attempt, long, long)
        assertTrue(line.length <= 200)
        assertTrue(line.contains("code=HOST_ERROR"))
        assertTrue(line.contains("exception=-"))
    }

    @Test fun lowercaseOrPunctuatedCodesFallBackWithoutEcho() {
        assertTrue(FailureDiagnostics.line("stop", "Idle", attempt, "transport_failed", null).contains("code=HOST_ERROR"))
        assertTrue(FailureDiagnostics.line("stop", "Idle", attempt, "A B C", null).contains("code=HOST_ERROR"))
    }
}

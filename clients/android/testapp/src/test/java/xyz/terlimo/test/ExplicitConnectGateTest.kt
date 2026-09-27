package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The pre-admission onboarding-hour intent is requested only from true user Connect.
 * These tests pin both the pure gate semantics and the source shape: exactly the
 * `select` (after VPN consent) and one-tap `autoConnectRetained` paths may arm it.
 */
class ExplicitConnectGateTest {
    private fun source(path: String): String = listOf(File(path), File("testapp/$path"), File("../$path"))
        .first { it.isFile }.readText()

    @Test fun onlyTrueConnectEntriesArm() {
        ExplicitConnectGate.Entry.entries.forEach { entry ->
            val expected = entry == ExplicitConnectGate.Entry.SELECT ||
                entry == ExplicitConnectGate.Entry.ONE_TAP_CONNECT ||
                entry == ExplicitConnectGate.Entry.PRE_ADMISSION
            assertEquals(entry.name, expected, ExplicitConnectGate.arms(entry))
        }
    }

    @Test fun resumeImportBackgroundRecoveryWakeSwitchAndProbeNeverArm() {
        val arming = ExplicitConnectArming()
        listOf(
            ExplicitConnectGate.Entry.RESUME,
            ExplicitConnectGate.Entry.IMPORT,
            ExplicitConnectGate.Entry.BACKGROUND,
            ExplicitConnectGate.Entry.RECOVERY,
            ExplicitConnectGate.Entry.WAKE,
            ExplicitConnectGate.Entry.SWITCH,
            ExplicitConnectGate.Entry.PROBE,
        ).forEach { entry ->
            assertFalse(entry.name, arming.arm(entry, "attempt-1"))
            assertFalse(entry.name, arming.armed("attempt-1"))
        }
    }

    @Test fun trueConnectArmsOnlyTheCurrentAttemptAndRejectsEmptyAttempt() {
        val arming = ExplicitConnectArming()
        assertFalse(arming.arm(ExplicitConnectGate.Entry.SELECT, null))
        assertFalse(arming.arm(ExplicitConnectGate.Entry.SELECT, ""))
        assertFalse(arming.armed(null))
        assertTrue(arming.arm(ExplicitConnectGate.Entry.SELECT, "a1"))
        assertTrue(arming.armed("a1"))
        assertFalse(arming.armed("a2"))
        assertTrue(arming.arm(ExplicitConnectGate.Entry.ONE_TAP_CONNECT, "a2"))
        assertTrue(arming.armed("a2"))
        assertFalse(arming.armed("a1"))
    }

    @Test fun stopClearsTheGate() {
        val arming = ExplicitConnectArming()
        arming.arm(ExplicitConnectGate.Entry.SELECT, "a1")
        arming.clear()
        assertFalse(arming.armed("a1"))
        arming.clear()
        assertFalse(arming.armed("a1"))
    }

    @Test fun sessionServiceArmsOnlyFromTheTrueConnectPaths() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        assertEquals(4, Regex("explicitConnect\\.arm\\(").findAll(service).count())
        assertTrue(service.contains("explicitConnect.arm(ExplicitConnectGate.Entry.SELECT, attempt)"))
        assertTrue(service.contains("explicitConnect.arm(ExplicitConnectGate.Entry.ONE_TAP_CONNECT, attempt)"))
        assertTrue(service.contains("explicitConnect.arm(ExplicitConnectGate.Entry.PRE_ADMISSION, attempt)"))
        assertEquals(4, Regex("ExplicitConnectGate\\.Entry\\.").findAll(service).count())
        ExplicitConnectGate.Entry.entries.filter { !it.explicit }.forEach { entry ->
            assertFalse(entry.name, service.contains("ExplicitConnectGate.Entry.${entry.name}"))
        }
        assertEquals(1, Regex("explicitConnect\\.clear\\(\\)").findAll(service).count())
        assertTrue(service.substringAfter("private fun stopAttempt(").contains("explicitConnect.clear()"))
    }

    @Test fun preAdmissionCommandIsAnExplicitFunnelNotBackgroundResume() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(service.contains("\"onboarding_connect\" ->"))
        assertTrue(service.contains("\"explicit_connect\""))
        assertFalse(service.contains("\"resume\"") && service.substringAfter("\"resume\" ->")
            .substringBefore("\"connect\" ->").contains("explicit_connect"))
    }
}

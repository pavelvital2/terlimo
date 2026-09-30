package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/**
 * Regression for the idle-navigation lifecycle defect: a tab switch with nothing to do
 * must not wake the service (and therefore must not promote an empty foreground
 * instance), while a running common ping and a live attempt keep their cancel/refresh
 * commands. Last-attempt diagnostics stay untouched.
 */
class NavigationServiceCommandsTest {

    private fun source(name: String): String =
        File("src/main/java/xyz/terlimo/test/$name").readText()

    @Test fun coldOffNavigationNeverWakesTheService() {
        // Leaving HOME with no common ping: nothing to cancel.
        assertFalse(NavigationServiceCommands.shouldCancelCommonPing(leavingHome = true, commonPingActive = false))
        // Opening HELP without a live attempt: nothing to ask for.
        assertFalse(NavigationServiceCommands.shouldRequestAnnouncements(openedHelp = true, attemptActive = false))
        // Staying on HOME or not opening HELP is never a command either.
        assertFalse(NavigationServiceCommands.shouldCancelCommonPing(leavingHome = false, commonPingActive = true))
        assertFalse(NavigationServiceCommands.shouldRequestAnnouncements(openedHelp = false, attemptActive = true))
    }

    @Test fun activeCommonPingAndLiveAttemptKeepTheirCommands() {
        assertTrue(NavigationServiceCommands.shouldCancelCommonPing(leavingHome = true, commonPingActive = true))
        assertTrue(NavigationServiceCommands.shouldRequestAnnouncements(openedHelp = true, attemptActive = true))
    }

    @Test fun onlyTheIdleNavigationCommandsAreClassified() {
        assertTrue(NavigationServiceCommands.isIdleCommand("probe_all_cancel"))
        assertTrue(NavigationServiceCommands.isIdleCommand("announcements_request"))
        for (action in listOf("refresh", "resume", "cancel", "probe_all", "probe", "select",
                "choose", "switch", "announcement_read", "autoconnect_cancel", null)) {
            assertFalse(action ?: "null", NavigationServiceCommands.isIdleCommand(action))
        }
        assertEquals("probe_all_cancel", NavigationServiceCommands.CANCEL_COMMON_PING)
        assertEquals("announcements_request", NavigationServiceCommands.REQUEST_ANNOUNCEMENTS)
    }

    @Test fun mainActivityGuardsNavigationAndProbeCancel() {
        val main = source("MainActivity.kt")
        assertTrue(main.contains("NavigationServiceCommands.shouldCancelCommonPing"))
        assertTrue(main.contains("NavigationServiceCommands.shouldRequestAnnouncements"))
        assertTrue(main.contains("SessionService.hasActiveCommonPing()"))
        assertTrue(main.contains("SessionService.hasLiveAttempt()"))
        // The old unconditional navigation wake-up pattern is gone.
        assertFalse(main.contains(
            "Leaving the catalog screen must stop an in-flight common ping.\n                    " +
                "startService(Intent(this@MainActivity, SessionService::class.java).setAction(\"probe_all_cancel\"))"))
    }

    @Test fun sessionServiceSkipsIdleCommandsBeforePromotingForeground() {
        val service = source("SessionService.kt")
        val onStart = service.substringAfter("override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {")
        val guard = onStart.indexOf("NavigationServiceCommands.isIdleCommand")
        val promote = onStart.indexOf("promoteToForeground()")
        assertTrue("idle guard must exist in onStartCommand", guard >= 0)
        assertTrue("idle guard must run before promotion", promote >= 0 && guard < promote)
        assertTrue("idle guard must require no live work", onStart.substring(guard, promote)
            .contains("gate.active == null && native == null"))
        // Announcements and common-ping state helpers stay the single source of truth.
        assertTrue(service.contains("internal fun hasLiveAttempt()"))
        assertTrue(service.contains("internal fun hasActiveCommonPing()"))
    }

    @Test fun lastAttemptDiagnosticsArePreserved() {
        val service = source("SessionService.kt")
        assertTrue(service.contains("lastReadiness = evidence.snapshot(probe.expired)"))
        assertTrue(service.contains("completedDiagnostics = UserDiagnostics.describe("))
    }
}

package xyz.terlimo.test

import java.io.File
import org.junit.Assert.*
import org.junit.Test

class MobileCatalogSourceTest {
    private fun source(path: String): String =
        listOf(File(path), File("testapp/$path"), File("../$path")).first { it.isFile }.readText()

    private fun service() = source("src/main/java/xyz/terlimo/test/SessionService.kt")

    private fun accountAccessBranch(): String {
        val service = service()
        return service.substringAfter("\"account_access\" -> {").substringBefore("\"telegram_registration\" ->") // scoped to the account_access case
    }

    @Test fun realHandleBranchDrivesTheStateMachineOnlyForAcceptedProjections() {
        val service = service()
        val branch = accountAccessBranch()
        assertTrue(branch.contains(
            "mobileCatalog.onAccountAccess(updated.projection.grant.dataAccess, attempt)"))
        assertTrue(branch.contains("catalogTimer.apply(attempt,"))
        val acceptedAt = branch.indexOf("updated != null")
        val transitionAt = branch.indexOf("mobileCatalog.onAccountAccess(")
        assertTrue("transition must sit inside the accepted-projection branch", acceptedAt in 1 until transitionAt)
        assertFalse("the retired stateless decision must be gone", service.contains("MobileCatalogGate.decide"))
        assertFalse("the shadow mobile-attempt flag must be gone", service.contains("mobileAttempt"))
    }

    @Test fun rightsTransitionsAreNeverTerminalAndNeverTouchTheCatalog() {
        val branch = accountAccessBranch()
        assertFalse(branch.contains("stopAttempt("))
        assertFalse(branch.contains("handleAttemptTerminal("))
        assertFalse(branch.contains("holdKillSwitch("))
        assertFalse(branch.contains("catalogTimer.clear("))
        assertFalse(branch.contains("onCatalogAccepted("))
        // S3-A adds the registration display projection to the same non-terminal publish.
        assertTrue(branch.contains("publishActive(attempt, view.copy(accountAccess = updated"))
    }

    @Test fun beginStartsTheAttemptThroughTheGateBeforeAnyChildStart() {
        val service = service()
        val startAt = service.indexOf("mobileCatalog.onAttemptStart(attempt, mobileBaseUrl != null)")
        val childStartAt = service.indexOf("child.start(start)")
        assertTrue("attempt-start transition must exist", startAt > 0)
        assertTrue("start transition must precede child start", startAt < childStartAt)
        assertTrue(service.contains("catalogTimer.apply(attempt, mobileCatalog.onAttemptStart"))
    }

    @Test fun catalogBranchAcceptsThroughTheGateAndStopResetsTheSameObject() {
        val service = service()
        val catalogHandler = service.substringAfter("\"catalog\" -> {").substringBefore("\"node_probe_result\" ->")
        assertTrue(catalogHandler.contains("mobileCatalog.onCatalogAccepted(attempt)"))
        assertTrue(catalogHandler.contains("catalogTimer.apply("))
        assertTrue(service.contains("mobileCatalog.onAttemptStop(gate.active)"))
        val stopHandler = service.substringAfter("private fun stopAttempt(code: String?) {")
            .substringBefore("retention.close()")
        assertTrue(stopHandler.contains(
            "if (mobileCatalog.onAttemptStop(gate.active) == MobileCatalogAction.DISARM) catalogTimer.clear()"))
    }

    @Test fun theOnlyCatalogueTimerIsTheInjectedBoundedWindow() {
        val service = service()
        assertTrue(service.contains("CatalogDeadlineTimer("))
        assertFalse("no shadow postDelayed catalogue timer in the service",
            service.contains("}, CatalogLoadTimeout.MILLIS)"))
        val timer = source("src/main/java/xyz/terlimo/test/CatalogDeadlineTimer.kt")
        assertTrue(timer.contains("CatalogLoadTimeout.MILLIS)"))
        assertTrue(timer.contains(
            "CatalogLoadTimeout.shouldStop(activeAttempt(), attempt, deadline.pending(attempt))"))
        assertTrue(timer.contains("armedWindow == window"))
        assertEquals(1, Regex("deadline\\.arm\\(").findAll(timer).count())
    }

    @Test fun exactlyOneBoundedMobileStartLineWithoutTokens() {
        val service = service()
        assertEquals(1, Regex("MobileAttemptDiagnostics\\.line\\(").findAll(service).count())
        assertTrue(service.contains("android.util.Log.i(MobileAttemptDiagnostics.TAG,"))
        assertTrue(service.contains("MobileAttemptDiagnostics.line(storage.installationId(), mobileBaseUrl)"))
    }
}

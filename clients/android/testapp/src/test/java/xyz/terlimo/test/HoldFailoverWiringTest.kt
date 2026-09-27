package xyz.terlimo.test

import java.nio.file.Files
import java.nio.file.Paths
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Wiring guard for UX §5 held-protection failover. The decisions are covered behaviourally by
 * HoldFailoverPolicyTest; this pins that the UI tap, the service admission and the apply-path
 * acceptance are actually connected, while the normal Connected switch stays untouched.
 */
class HoldFailoverWiringTest {
    private fun source(path: String) = String(Files.readAllBytes(Paths.get(path)), Charsets.UTF_8)

    @Test fun killSwitchTapSendsTheHeldFailoverAction() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        assertTrue(activity.contains("\"KillSwitch\" -> \"hold_select\""))
        assertTrue(activity.contains("state.phase !in setOf(\"CatalogReady\", \"Connected\", \"KillSwitch\")"))
    }

    @Test fun serviceAdmitsAndAppliesOnlyTheArmedHeldTarget() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(service.contains("\"hold_select\" ->"))
        assertTrue(service.contains("HoldFailoverPolicy.tap("))
        assertTrue(service.contains("HoldFailoverPolicy.childStopped("))
        assertTrue(service.contains("HoldFailoverPolicy.catalogTarget("))
        assertTrue(service.contains("val holdSelect = holdFailover.attempt == attempt && holdFailover.nodeId == nodeId"))
        assertTrue(service.contains("switching || holdSelect ||"))
        assertTrue(service.contains("clearHoldFailover()"))
    }

    @Test fun startWaitsForTheConfirmedChildStopOfTheCurrentGeneration() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(service.contains("private fun startHeldFailover(target: String)"))
        assertTrue(service.contains("private fun completeHoldFailover(stopGeneration: Long, stopConfirmed: Boolean)"))
        assertTrue(service.contains("holdStopGeneration++"))
        assertTrue(service.contains("val stopped = child?.stop(ChildStopReason.KILL_SWITCH_HOLD, stopPhase) ?: true"))
        assertTrue(service.contains("HoldFailoverPolicy.completionApplies(stopGeneration, holdStopGeneration, stopping.get())"))
        assertTrue(service.contains("recoveryNativeStopped = stopped"))
        assertTrue(service.contains("recoveryNativeStopped && activeVpnConfig != null"))
        assertTrue(service.contains("invalidateHoldFailover()"))
        assertTrue(service.contains("HoldFailoverPolicy.cleared()"))
    }

    @Test fun normalConnectedSwitchIsUnchanged() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(service.contains("ActiveNodeSwitch.admission(view, id)"))
        assertTrue(service.contains("\"switch_node\""))
        assertTrue(service.contains("ActiveNodeSwitch.awaitingConfig("))
    }
}

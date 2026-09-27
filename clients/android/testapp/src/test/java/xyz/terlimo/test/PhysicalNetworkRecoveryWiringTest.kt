package xyz.terlimo.test

import java.nio.file.Files
import java.nio.file.Paths
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class PhysicalNetworkRecoveryWiringTest {
    private val source = String(Files.readAllBytes(Paths.get("src/main/java/xyz/terlimo/test/SessionService.kt")))

    @Test fun callbacksRequireValidatedDefaultPhysicalNetwork() {
        assertTrue(source.contains("override fun onLost(lost: Network)"))
        assertTrue(source.contains("override fun onAvailable(network: Network)"))
        assertTrue(source.contains("override fun onCapabilitiesChanged("))
        assertTrue(source.contains("NET_CAPABILITY_VALIDATED"))
        assertTrue(source.contains("connectivity.activeNetwork == network"))
        assertTrue(source.contains("if (network == physical && PhysicalNetworkRecovery.boundPathUnavailable(candidate))"))
    }

    @Test fun recoveryKeepsVpnOwnerAndUsesOnlySavedNode() {
        val recovery = source.substringAfter("private fun beginPhysicalRecovery()")
            .substringBefore("private fun stopAttempt")
        assertTrue(recovery.contains("val nodeId = networkRecovery?.nodeId ?: activeVpnNodeId"))
        assertTrue(recovery.contains("!PhysicalNetworkRecovery.recoveryAllowed(view.phase, networkRecovery != null)"))
        assertTrue(recovery.contains("gate.cancel()"))
        assertTrue(recovery.contains("child?.stop("))
        val gateAt = recovery.indexOf("gate.cancel()")
        val childStopAt = recovery.indexOf("child?.stop(")
        assertTrue(gateAt >= 0)
        assertTrue(childStopAt >= 0)
        assertTrue(gateAt < childStopAt)
        assertFalse(recovery.contains("setState(tunnel, Tunnel.State.DOWN"))
        assertFalse(recovery.contains("NodeSelection.first"))
        assertTrue(source.contains("PhysicalNetworkRecovery.established("))
    }

    @Test fun sleepResumeStartsRecoveryOnlyAfterNativeStop() {
        val resume = source.substringAfter("private fun resumeFromSleep()")
            .substringBefore("private fun handlePhysicalLoss")
        val readyAt = resume.indexOf("if (!recoveryNativeStopped) return")
        val startAt = resume.indexOf("beginPhysicalRecovery()")
        assertTrue(readyAt >= 0)
        assertTrue(startAt >= 0)
        assertTrue(readyAt < startAt)
    }

    @Test fun recoveryAdmitsOnlyConnectedSwitchingOrSleepPaused() {
        assertTrue(PhysicalNetworkRecovery.recoveryAllowed("Connected", hasRecovery = false))
        assertTrue(PhysicalNetworkRecovery.recoveryAllowed("SwitchingServer", hasRecovery = false))
        assertTrue(PhysicalNetworkRecovery.recoveryAllowed("SleepPaused", hasRecovery = false))
        assertFalse(PhysicalNetworkRecovery.recoveryAllowed("Idle", hasRecovery = false))
        assertFalse(PhysicalNetworkRecovery.recoveryAllowed("Error", hasRecovery = false))
        assertTrue(PhysicalNetworkRecovery.recoveryAllowed("Idle", hasRecovery = true))
    }

    @Test fun recoveryIsGenerationFencedAndBounded() {
        assertTrue(source.contains("PhysicalNetworkRecovery.scheduledStartOutcome(state, start.state.generation,"))
        assertTrue(source.contains("main.postDelayed(recoveryExpiry, PhysicalNetworkRecovery.WINDOW_MS)"))
        assertTrue(source.contains("PhysicalNetworkRecovery.shouldSelectNode(recovery, attempt"))
        assertTrue(source.contains("recoveryConnectAttempt = attempt"))
        assertTrue(source.contains("PhysicalNetworkRecovery.admit(state)"))
        assertTrue(source.contains("recovery.inFlight || gate.active != null"))
    }

    @Test fun retryableRecoveryTerminalKeepsLastGoodUntilBoundedRetriesExhaust() {
        val retry = source.substringAfter("private fun handleRecoveryTerminal(")
            .substringBefore("private fun stopAttempt")
        assertTrue(retry.contains("PhysicalNetworkRecovery.retry("))
        assertTrue(retry.indexOf("gate.cancel()") < retry.indexOf("child?.stop("))
        assertTrue(retry.contains("current.generation != generation"))
        assertTrue(retry.contains("holdKillSwitch(code)"))
        assertFalse(retry.contains("Tunnel.State.DOWN"))
    }

    @Test fun exhaustionKeepsVpnKillSwitchUntilExplicitCancel() {
        val hold = source.substringAfter("private fun holdKillSwitch(").substringBefore("private fun stopAttempt")
        assertTrue(hold.contains("child?.stop("))
        assertTrue(hold.contains("gate.cancel()"))
        assertTrue(hold.contains("phase = \"KillSwitch\""))
        assertFalse(hold.contains("Tunnel.State.DOWN"))
        assertFalse(hold.contains("stopSelf()"))
        assertTrue(source.contains("\"cancel\" -> stopAttempt(null)"))
    }

    @Test fun terminalFailuresUsePolicyAndLongLossNeverCancelsRecovery() {
        assertTrue(source.contains("TerminalFailurePolicy.outcome(code, recovery != null, activeVpnConfig != null)"))
        assertTrue(source.contains("TerminalFailurePolicy.decide(attempt, active, code, networkRecovery != null, activeVpnConfig != null)"))
        assertTrue(source.contains("private fun awaitRecoveryNetwork("))
        assertTrue(source.contains("PhysicalNetworkRecovery.await("))
        assertTrue(source.contains("PhysicalNetworkRecovery.scheduledStartOutcome(state, start.state.generation,"))
        assertTrue(source.contains("PhysicalNetworkRecovery.expired(recovery, now)"))
        assertTrue(source.contains("PhysicalNetworkRecovery.resume(recovery, recovery.generation, candidate, now"))
        // A resumed recovery or held-protection attempt must not release the retained protection.
        assertTrue(source.contains("} else if (networkRecovery != null || activeVpnConfig != null) {"))
        assertTrue(source.contains("handleAttemptTerminal(attempt, terminalCode)"))
        // The old unbounded-path bug: window expiry must not kill the recovery state.
        assertFalse(source.contains("if (networkRecovery != null) holdKillSwitch(\"PHYSICAL_NETWORK_UNAVAILABLE\")"))
    }
}

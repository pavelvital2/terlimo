package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class PhysicalNetworkRecoveryTest {
    @Test fun lossHoldsKillSwitchAndNeverChangesSelectedNode() {
        val recovery = PhysicalNetworkRecovery.begin("node-a", generation = 7, nowElapsed = 1_000)
        assertEquals("node-a", recovery.nodeId)
        assertEquals(8, recovery.generation)
        assertTrue(recovery.killSwitchHeld)
        assertEquals("node-a", PhysicalNetworkRecovery.ready(recovery, generation = 8,
            NetworkCandidate(internet = true, notVpn = true, validated = false, isDefault = true), 1_100)?.state?.nodeId)
    }

    @Test fun onlyPhysicalInternetNetworkCanStartReadmission() {
        val recovery = PhysicalNetworkRecovery.begin("node-b", generation = 2, nowElapsed = 5_000)
        val valid = NetworkCandidate(internet = true, notVpn = true, validated = true, isDefault = true)
        assertEquals(0L, PhysicalNetworkRecovery.ready(recovery, 3, valid, 5_001)?.delayMs)
        for (bad in listOf(valid.copy(internet = false), valid.copy(notVpn = false))) {
            assertNull(PhysicalNetworkRecovery.ready(recovery, 3, bad, 5_001))
        }
        assertEquals(0L, PhysicalNetworkRecovery.ready(recovery, 3,
            valid.copy(validated = false, isDefault = false), 5_001)?.delayMs)
    }

    @Test fun staleGenerationAndExpiredWindowFailClosed() {
        val recovery = PhysicalNetworkRecovery.begin("node-a", generation = 10, nowElapsed = 1_000)
        val valid = NetworkCandidate(true, true, true, true)
        assertNull(PhysicalNetworkRecovery.ready(recovery, 10, valid, 1_100))
        assertNull(PhysicalNetworkRecovery.ready(recovery, 12, valid, recovery.deadlineElapsed + 1))
        assertFalse(PhysicalNetworkRecovery.established(recovery, generation = 10, nodeId = "node-a"))
        assertFalse(PhysicalNetworkRecovery.established(recovery, generation = 11, nodeId = "node-b"))
        assertTrue(PhysicalNetworkRecovery.established(recovery, generation = 11, nodeId = "node-a"))
    }

    @Test fun retryIsBoundedAndKeepsExactNode() {
        var recovery = PhysicalNetworkRecovery.begin("node-b", generation = 20, nowElapsed = 10_000)
        assertEquals(listOf(0L, 1_000L, 3_000L), (1..3).map {
            val next = PhysicalNetworkRecovery.retry(recovery, 10_100) ?: error("missing retry")
            recovery = next.state
            assertEquals("node-b", recovery.nodeId)
            next.delayMs
        })
        assertNull(PhysicalNetworkRecovery.retry(recovery, 10_100))
    }

    @Test fun activeAttemptDoesNotConsumeRetryAndNextAttemptCanStart() {
        val valid = NetworkCandidate(true, true, true, true)
        val initial = PhysicalNetworkRecovery.begin("node-a", generation = 30, nowElapsed = 20_000)
        val first = PhysicalNetworkRecovery.ready(initial, 31, valid, 20_001) ?: error("first")
        val active = PhysicalNetworkRecovery.admit(first.state)
        assertNull(PhysicalNetworkRecovery.ready(active, 31, valid, 20_002))
        assertEquals(1, active.attempt)

        val second = PhysicalNetworkRecovery.retry(active.copy(inFlight = false), 20_100) ?: error("second")
        assertEquals(2, second.state.attempt)
        assertEquals("node-a", second.state.nodeId)
    }

    @Test fun retryCatalogSelectsRetainedNodeOncePerNativeAttempt() {
        val recovery = PhysicalNetworkRecovery.begin("node-b", generation = 40, nowElapsed = 30_000)
        val nodes = setOf("node-a", "node-b")
        assertTrue(PhysicalNetworkRecovery.shouldSelectNode(
            recovery, "attempt-1", null, "CatalogReady", "node-b", nodes))
        assertFalse(PhysicalNetworkRecovery.shouldSelectNode(
            recovery, "attempt-1", "attempt-1", "CatalogReady", "node-b", nodes))
        assertTrue(PhysicalNetworkRecovery.shouldSelectNode(
            recovery, "attempt-2", "attempt-1", "CatalogReady", "node-b", nodes))
    }

    @Test fun longLossAwaitsAndResumesSameNodeOnUsableNetwork() {
        var recovery = PhysicalNetworkRecovery.begin("node-a", generation = 50, nowElapsed = 100_000)
        // The 30 s series ends with no suitable physical path: no retry is possible.
        assertNull(PhysicalNetworkRecovery.ready(recovery, 51,
            NetworkCandidate(false, false, false, false), 130_001))

        recovery = PhysicalNetworkRecovery.await(recovery)
        assertTrue(recovery.awaitingNetwork)
        assertEquals("node-a", recovery.nodeId)
        assertEquals(51, recovery.generation)

        // A usable network returns: a fresh series for the SAME node starts immediately.
        val resumed = PhysicalNetworkRecovery.resume(recovery, generation = 51,
            NetworkCandidate(true, true, true, true), nowElapsed = 200_000,
            leaseValid = true, stopping = false) ?: error("resume expected")
        assertFalse(resumed.state.awaitingNetwork)
        assertEquals(52, resumed.state.generation)
        assertEquals(0, resumed.state.attempt)
        assertEquals("node-a", resumed.state.nodeId)
        assertEquals(200_000 + PhysicalNetworkRecovery.WINDOW_MS, resumed.state.deadlineElapsed)
        assertEquals(0L, resumed.delayMs)

        // The resumed series uses the standard bounded retry rules.
        val first = PhysicalNetworkRecovery.ready(resumed.state.copy(inFlight = false), 52,
            NetworkCandidate(true, true, false, false), 200_001) ?: error("first retry expected")
        assertEquals(1, first.state.attempt)
        assertEquals(0L, first.delayMs)
    }

    @Test fun awaitResumeFencesStopLeaseGenerationAndUnusableNetworks() {
        val awaited = PhysicalNetworkRecovery.await(PhysicalNetworkRecovery.begin("node-b", 60, 1_000))
        val usable = NetworkCandidate(true, true, true, true)
        // Explicit Disconnect cancels any further recovery.
        assertNull(PhysicalNetworkRecovery.resume(awaited, 61, usable, 2_000, leaseValid = true, stopping = true))
        // Expired or revoked rights never restart.
        assertNull(PhysicalNetworkRecovery.resume(awaited, 61, usable, 2_000, leaseValid = false, stopping = false))
        // Stale generation callbacks are ignored.
        assertNull(PhysicalNetworkRecovery.resume(awaited, 60, usable, 2_000, leaseValid = true, stopping = false))
        // Only internet non-VPN candidates may resume.
        assertNull(PhysicalNetworkRecovery.resume(awaited, 61,
            NetworkCandidate(true, false, true, true), 2_000, leaseValid = true, stopping = false))
        assertNull(PhysicalNetworkRecovery.resume(awaited, 61,
            NetworkCandidate(false, true, true, true), 2_000, leaseValid = true, stopping = false))
        // A series that is not awaiting and still inside its window never resumes.
        val active = PhysicalNetworkRecovery.begin("node-b", 70, 3_000)
        assertNull(PhysicalNetworkRecovery.resume(active, 71, usable, 3_100, leaseValid = true, stopping = false))
    }

    @Test fun cancelledDelayedStartWithoutUsableNetworkEntersWaitAndResumesSameNode() {
        val state = PhysicalNetworkRecovery.begin("node-a", generation = 90, nowElapsed = 10_000)
        // D2 regression: the delayed start is cancelled because the candidate disappeared
        // after the deadline; it must enter the waiting state, not silently vanish.
        assertEquals(RecoveryStartOutcome.AWAIT, PhysicalNetworkRecovery.scheduledStartOutcome(
            state, 91, stopping = false, activeAttempt = false, usablePath = false))
        assertEquals(RecoveryStartOutcome.START, PhysicalNetworkRecovery.scheduledStartOutcome(
            state, 91, stopping = false, activeAttempt = false, usablePath = true))
        assertEquals(RecoveryStartOutcome.DISCARD, PhysicalNetworkRecovery.scheduledStartOutcome(
            state, 91, stopping = true, activeAttempt = false, usablePath = true))
        assertEquals(RecoveryStartOutcome.DISCARD, PhysicalNetworkRecovery.scheduledStartOutcome(
            state, 91, stopping = false, activeAttempt = true, usablePath = true))
        assertEquals(RecoveryStartOutcome.DISCARD, PhysicalNetworkRecovery.scheduledStartOutcome(
            state, 92, stopping = false, activeAttempt = false, usablePath = true))
        assertEquals(RecoveryStartOutcome.DISCARD, PhysicalNetworkRecovery.scheduledStartOutcome(
            null, 91, stopping = false, activeAttempt = false, usablePath = true))

        val resumed = PhysicalNetworkRecovery.resume(PhysicalNetworkRecovery.await(state), 91,
            NetworkCandidate(true, true, true, true), 40_001, leaseValid = true, stopping = false)
            ?: error("resume expected")
        assertEquals("node-a", resumed.state.nodeId)
        assertEquals(92, resumed.state.generation)
        assertEquals(0L, resumed.delayMs)
    }

    @Test fun expiredSeriesWithoutAwaitFlagCanStillResumeSameNodeButNeverInFlight() {
        val state = PhysicalNetworkRecovery.begin("node-b", generation = 95, nowElapsed = 1_000)
        val usable = NetworkCandidate(true, true, true, true)
        val resumed = PhysicalNetworkRecovery.resume(state, 96, usable, 31_001,
            leaseValid = true, stopping = false) ?: error("expired series must resume")
        assertEquals("node-b", resumed.state.nodeId)
        assertEquals(97, resumed.state.generation)
        // An in-flight attempt never restarts, and a non-expired series is owned by ready/retry.
        assertNull(PhysicalNetworkRecovery.resume(state.copy(inFlight = true), 96, usable, 31_001,
            leaseValid = true, stopping = false))
        assertNull(PhysicalNetworkRecovery.resume(state, 96, usable, 2_000,
            leaseValid = true, stopping = false))
    }
}

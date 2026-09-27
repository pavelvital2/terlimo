package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Behavioural tests for UX §5 held-protection failover on the real decision state used by
 * SessionService: a tap while the previous native child is still stopping must wait for the
 * confirmed completion of the current stop generation and then start exactly once; failures,
 * stale completions and cancellations never start anything.
 */
class HoldFailoverPolicyTest {
    private fun tap(state: HoldFailoverState, target: String, phase: String = "KillSwitch",
        protection: Boolean = true, targetInCatalog: Boolean = true, lease: Boolean = true,
        nativeStopped: Boolean = true, generation: Long = 7) =
        HoldFailoverPolicy.tap(state, target, phase, protection, targetInCatalog, lease, nativeStopped, generation)

    private fun childStopped(state: HoldFailoverState, generation: Long = 7, confirmed: Boolean = true,
        stopping: Boolean = false, phase: String = "KillSwitch", protection: Boolean = true,
        targetInCatalog: Boolean = true, lease: Boolean = true) =
        HoldFailoverPolicy.childStopped(state, generation, confirmed, stopping, phase, protection,
            targetInCatalog, lease)

    // --- admission ---

    @Test fun heldProtectionAllowsAnotherGatewayWithoutDisconnect() {
        val (state, decision) = tap(HoldFailoverState(), "node-b")
        assertTrue(decision is HoldFailoverStart.Start)
        assertEquals("node-b", state.armed)
    }

    @Test fun onlyTheHeldStateMayStartTheFailover() {
        for (phase in listOf("Idle", "Error", "CatalogReady", "Connected", "SwitchingServer",
                "Reconnecting", "ConfiguringVPN", "SleepPaused", "Stopping")) {
            val (_, decision) = tap(HoldFailoverState(), "node-b", phase = phase)
            assertEquals(phase, HoldFailoverStart.Rejected, decision)
        }
    }

    @Test fun withoutAppliedProtectionNothingIsStarted() {
        val (_, decision) = tap(HoldFailoverState(), "node-b", protection = false)
        assertEquals(HoldFailoverStart.Rejected, decision)
    }

    @Test fun targetMustExistInTheVerifiedCatalogAndAccessMustBeValid() {
        assertEquals(HoldFailoverStart.Rejected, tap(HoldFailoverState(), "node-b", targetInCatalog = false).second)
        assertEquals(HoldFailoverStart.Rejected, tap(HoldFailoverState(), "node-b", lease = false).second)
    }

    // --- child-stop wait and exactly-once start ---

    @Test fun tapWhilePreviousChildStillStoppingWaitsThenStartsExactlyOnce() {
        val (waiting, decision) = tap(HoldFailoverState(), "node-b", nativeStopped = false)
        assertTrue(decision is HoldFailoverStart.Wait)
        assertEquals(7L, (decision as HoldFailoverStart.Wait).stopGeneration)
        assertTrue(waiting.pending)

        // A repeated tap while the choice is pending is rejected.
        val (stillWaiting, repeated) = tap(waiting, "node-c", nativeStopped = false)
        assertEquals(HoldFailoverStart.Rejected, repeated)
        assertEquals(waiting, stillWaiting)

        // Completion starts exactly once; a second completion cannot start again and another
        // tap stays rejected while the armed start is in flight.
        val (started, completion) = childStopped(stillWaiting)
        assertTrue(completion is HoldFailoverStart.Start)
        assertEquals("node-b", (completion as HoldFailoverStart.Start).target)
        assertEquals("node-b", started.armed)
        assertNull(started.wait)
        val (stillArmed, rejectedTap) = tap(started, "node-c")
        assertEquals(HoldFailoverStart.Rejected, rejectedTap)
        assertEquals(started, stillArmed)
        val (after, duplicate) = childStopped(started)
        assertEquals(HoldFailoverStart.Ignored, duplicate)
        assertEquals(started, after)
    }

    @Test fun completionBeforeTapIsIgnoredAndTheLaterTapStartsImmediately() {
        val (state, ignored) = childStopped(HoldFailoverState())
        assertEquals(HoldFailoverStart.Ignored, ignored)
        assertFalse(state.pending)
        val (armed, decision) = tap(state, "node-b", nativeStopped = true)
        assertTrue(decision is HoldFailoverStart.Start)
        assertEquals("node-b", armed.armed)
    }

    @Test fun staleCompletionNeverUnblocksANewCycle() {
        val (waiting, _) = tap(HoldFailoverState(), "node-b", nativeStopped = false, generation = 7)
        // Completion of an older stop generation is ignored and leaves the pending choice.
        val (unchanged, stale) = childStopped(waiting, generation = 6)
        assertEquals(HoldFailoverStart.Ignored, stale)
        assertEquals(waiting, unchanged)
        // The matching generation starts.
        val (_, fresh) = childStopped(unchanged, generation = 7)
        assertTrue(fresh is HoldFailoverStart.Start)
    }

    @Test fun failedOrUnconfirmedStopNeverStartsAndNeverRetries() {
        val (waiting, _) = tap(HoldFailoverState(), "node-b", nativeStopped = false)
        val (state, decision) = childStopped(waiting, confirmed = false)
        assertEquals(HoldFailoverStart.Rejected, decision)
        assertFalse(state.pending)
        assertNull(state.armed)
    }

    @Test fun changedHoldConditionsCancelThePendingChoiceOnCompletion() {
        val (waiting, _) = tap(HoldFailoverState(), "node-b", nativeStopped = false)
        for ((name, result) in listOf(
            "stopping" to childStopped(waiting, stopping = true),
            "phase" to childStopped(waiting, phase = "Reconnecting"),
            "protection" to childStopped(waiting, protection = false),
            "target" to childStopped(waiting, targetInCatalog = false),
            "lease" to childStopped(waiting, lease = false),
        )) {
            assertEquals(name, HoldFailoverStart.Rejected, result.second)
            assertFalse(name, result.first.pending)
        }
    }

    @Test fun disconnectOrExpiryCancelsThePendingChoiceWithoutRevival() {
        val (waiting, _) = tap(HoldFailoverState(), "node-b", nativeStopped = false)
        assertTrue(waiting.pending)
        val cleared = HoldFailoverPolicy.cleared()
        // A late completion after the cancel is ignored and starts nothing.
        val (state, decision) = childStopped(cleared)
        assertEquals(HoldFailoverStart.Ignored, decision)
        assertFalse(state.pending)
        // A fresh tap after the cancel is admitted again.
        val (_, again) = tap(state, "node-b")
        assertTrue(again is HoldFailoverStart.Start)
    }

    // --- catalog / failure ---

    @Test fun freshCatalogSelectsOnlyTheArmedTargetAndNeverFallsBack() {
        val catalog = setOf("node-a", "node-b")
        assertEquals("node-b", HoldFailoverPolicy.catalogTarget("node-b", catalog))
        assertNull(HoldFailoverPolicy.catalogTarget("node-c", catalog))
        assertNull(HoldFailoverPolicy.catalogTarget(null, catalog))
    }

    @Test fun acceptedCatalogBindsTheAttemptAndClearsTheArmedTarget() {
        val (waiting, _) = tap(HoldFailoverState(), "node-b")
        val accepted = HoldFailoverPolicy.catalogAccepted(waiting, "attempt-1", "node-b")
        assertNull(accepted.armed)
        assertEquals("attempt-1", accepted.attempt)
        assertEquals("node-b", accepted.nodeId)
        assertFalse(accepted.pending)
    }

    @Test fun failedHeldAttemptKeepsProtectionAndAvailableChoice() {
        assertEquals(TerminalFailureOutcome.HOLD, HoldFailoverPolicy.failureOutcome(protectionApplied = true))
        assertEquals(TerminalFailureOutcome.STOP, HoldFailoverPolicy.failureOutcome(protectionApplied = false))
    }

    @Test fun staleCompletionNeverChangesTheNewStopReadiness() {
        // A completion of an invalidated/older stop generation or a stopping owner changes nothing.
        assertFalse(HoldFailoverPolicy.completionApplies(stopGeneration = 7, currentStopGeneration = 9, stopping = false))
        assertFalse(HoldFailoverPolicy.completionApplies(stopGeneration = 9, currentStopGeneration = 9, stopping = true))
        assertFalse(HoldFailoverPolicy.completionApplies(stopGeneration = 7, currentStopGeneration = 9, stopping = true))
        // Only the current, non-stopping generation may publish the stop readiness.
        assertTrue(HoldFailoverPolicy.completionApplies(stopGeneration = 9, currentStopGeneration = 9, stopping = false))
    }
}

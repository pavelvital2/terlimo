package xyz.terlimo.test

import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/**
 * §26.5 single coordination boundary. Executes the production API (`setMode`, `registerJob`,
 * `pendCold`, `beginCycle`, `cancelJob`, `commit`) including the two orderings of Off against an
 * in-flight commit, queued-dispatch invalidation, duplicate job-only events and manual takeover.
 */
class CatalogRefreshCoordinatorTest {

    private val gate = CatalogRefreshCoordinator()

    @After fun tearDown() = gate.resetForTest()

    private class Recorder {
        var committed = false
        val block: () -> Unit = { committed = true }
    }

    private fun onAndRegistered(id: String): Boolean {
        gate.setMode(CatalogRefreshMode.DAILY)
        return gate.registerJob(id, gate.currentEpoch())
    }

    @Test fun offWaitsForAnInFlightCommitAndCannotSplitIt() {
        assertTrue(onAndRegistered("j1"))
        assertTrue(gate.pendCold("j1"))
        assertEquals(CatalogRefreshCoordinator.Origin.JOB, gate.beginCycle("a1"))
        val entered = CountDownLatch(1)
        val release = CountDownLatch(1)
        var committed = false
        var change: CatalogRefreshCoordinator.ScheduleChange? = null
        val committer = Thread {
            gate.commit("a1") {
                committed = true
                entered.countDown()
                release.await(5, TimeUnit.SECONDS)
            }
        }
        committer.start()
        assertTrue(entered.await(5, TimeUnit.SECONDS))
        val switcher = Thread { change = gate.setMode(CatalogRefreshMode.OFF) }
        switcher.start()
        Thread.sleep(150)
        assertTrue("setMode must block until the commit block finished", change == null)
        release.countDown()
        committer.join(5000)
        switcher.join(5000)
        assertTrue(committed)
        assertTrue(change!!.applied)
        assertTrue("Off after the commit must not fail the completed job", change!!.jobFailures.isEmpty())
    }

    @Test fun offBeforeCommitSuppressesTheWriteAndStopsTheJobOnlyCycle() {
        assertTrue(onAndRegistered("j1"))
        assertTrue(gate.pendCold("j1"))
        assertEquals(CatalogRefreshCoordinator.Origin.JOB, gate.beginCycle("a1"))
        val change = gate.setMode(CatalogRefreshMode.OFF)
        assertEquals(listOf("j1"), change.jobFailures.map { it.requestId })
        assertEquals(listOf("a1"), change.attemptsToStop)
        val rec = Recorder()
        val plan = gate.commit("a1", rec.block)
        assertFalse(plan.publish)
        assertTrue(plan.stopCycle)
        assertFalse(rec.committed)
    }

    @Test fun commitOfAValidJobOnlyCyclePublishesAndTheDuplicateIsRejected() {
        assertTrue(onAndRegistered("j1"))
        assertTrue(gate.pendCold("j1"))
        gate.beginCycle("a1")
        val first = gate.commit("a1", {})
        assertTrue(first.publish)
        assertEquals(listOf("j1"), first.completions.map { it.requestId })
        val rec = Recorder()
        val duplicate = gate.commit("a1", rec.block)
        assertFalse(duplicate.publish)
        assertTrue(duplicate.duplicate)
        assertFalse(duplicate.stopCycle)
        assertFalse("a duplicate job-only event must never publish", rec.committed)
    }

    @Test fun canceledJobOnlyCycleKeepsItsRecordUntilTeardown() {
        assertTrue(onAndRegistered("j1"))
        assertTrue(gate.pendCold("j1"))
        gate.beginCycle("a1")
        val cancel = gate.cancelJob("j1")
        assertEquals(listOf("a1"), cancel.attemptsToStop)
        assertTrue(gate.isJobOnly("a1"))
        val rec = Recorder()
        assertFalse(gate.commit("a1", rec.block).publish)
        assertTrue("record survives the suppressed commit", gate.isJobOnly("a1"))
        gate.planTerminal("a1", "VPN_STOPPED")
        assertFalse(gate.isJobOnly("a1"))
    }

    @Test fun manualTakeoverBeforeCancelKeepsTheCycleAlive() {
        assertTrue(onAndRegistered("j1"))
        assertTrue(gate.pendCold("j1"))
        gate.beginCycle("a1")
        gate.markAttemptManual("a1")
        val cancel = gate.cancelJob("j1")
        assertTrue(cancel.attemptsToStop.isEmpty())
        val rec = Recorder()
        val plan = gate.commit("a1", rec.block)
        assertTrue(plan.publish)
        assertTrue(rec.committed)
        assertTrue(plan.completions.isEmpty())
    }

    @Test fun manualRefreshMergedWithAJobCompletesBoth() {
        assertTrue(onAndRegistered("j1"))
        assertTrue(gate.pendCold("j1"))
        gate.markServiceIntent()
        assertEquals(CatalogRefreshCoordinator.Origin.MANUAL, gate.beginCycle("a1"))
        val rec = Recorder()
        val plan = gate.commit("a1", rec.block)
        assertTrue(plan.publish)
        assertTrue(rec.committed)
        assertEquals(listOf("j1"), plan.completions.map { it.requestId })
    }

    @Test fun offWithManualOwnerFailsTheJobButKeepsTheCycle() {
        assertTrue(onAndRegistered("j1"))
        assertTrue(gate.pendCold("j1"))
        gate.markServiceIntent()
        gate.beginCycle("a1")
        val change = gate.setMode(CatalogRefreshMode.OFF)
        assertEquals(listOf("j1"), change.jobFailures.map { it.requestId })
        assertTrue(change.attemptsToStop.isEmpty())
        val rec = Recorder()
        assertTrue(gate.commit("a1", rec.block).publish)
        assertTrue(rec.committed)
    }

    @Test fun queuedColdDispatchIsInvalidatedByACancelThatHappensFirst() {
        assertTrue(onAndRegistered("j1"))
        assertTrue(gate.pendCold("j1"))
        // The cancel lands before the queued begin task is drained.
        gate.cancelJob("j1")
        var dispatched = false
        // drain: the queued begin re-validates the claim on the actor.
        if (gate.isColdJobValid("j1")) {
            gate.beginCycle("a1")
            dispatched = true
        }
        assertFalse(dispatched)
        assertFalse(gate.isJobOnly("a1"))
    }

    @Test fun staleJobStartEpochIsRejectedAtRegistration() {
        gate.setMode(CatalogRefreshMode.DAILY)
        val epochBefore = gate.currentEpoch()
        gate.setMode(CatalogRefreshMode.WEEKLY)
        assertFalse(gate.registerJob("j1", epochBefore))
        assertTrue(gate.registerJob("j2", gate.currentEpoch()))
    }

    @Test fun registrationIsRejectedWhileOff() {
        gate.setMode(CatalogRefreshMode.OFF)
        assertFalse(gate.registerJob("j1", gate.currentEpoch()))
    }

    @Test fun modeChangeRejectsAnUnattachedRequestWithoutResurrectingIt() {
        assertTrue(onAndRegistered("j1"))
        gate.beginCycle("a1")
        val change = gate.setMode(CatalogRefreshMode.WEEKLY)
        assertEquals(listOf("j1"), change.jobFailures.map { it.requestId })
        assertFalse(gate.attach("a1", "j1"))
        assertFalse(gate.dispatch("a1", "j1") { error("must not send") })
        assertTrue(gate.setMode(CatalogRefreshMode.OFF).jobFailures.isEmpty())
        assertFalse(gate.cancelJob("j1").found)
    }

    @Test fun terminalReportsFailuresAndForgetsTheAttempt() {
        assertTrue(onAndRegistered("j1"))
        assertTrue(gate.pendCold("j1"))
        gate.beginCycle("a1")
        val completions = gate.planTerminal("a1", "CATALOG_TIMEOUT")
        assertEquals(listOf("j1"), completions.map { it.requestId })
        assertFalse(completions.single().ok)
        gate.setMode(CatalogRefreshMode.OFF)
        gate.setMode(CatalogRefreshMode.DAILY)
        assertTrue("no double report for a forgotten request", gate.cancelJob("j1").found.not())
    }

    @Test fun manualCycleStillAcceptsSubsequentCatalogs() {
        gate.beginCycle("manual")
        var writes = 0
        repeat(2) { assertTrue(gate.commit("manual") { writes++ }.publish) }
        assertEquals(2, writes)
    }

    @Test fun unknownAttemptStillPublishesForTheManualPath() {
        val rec = Recorder()
        val plan = gate.commit("foreign", rec.block)
        assertTrue(plan.publish)
        assertTrue(rec.committed)
    }
}

package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * §26.5 job-run glue ordering. Executes the sequences that matter on one Service instance:
 * two consecutive runs, late callbacks/timeouts of an old run after a new run started,
 * onStopJob for a stale JobParameters, bind exceptions and schedule-off.
 */
class CatalogRefreshJobCoordinatorTest {

    private class FakeActions : CatalogRefreshJobCoordinator.Actions {
        val bound = mutableListOf<Long>()
        val unbound = mutableListOf<Long>()
        val finished = mutableListOf<Long>()
        val discarded = mutableListOf<Long>()
        val requests = mutableListOf<Pair<Long, String>>()
        val canceled = mutableListOf<Pair<Long, String>>()
        var acceptRequests = true
        var bindResult = true
        override fun bind(token: Long): Boolean {
            if (bindResult) bound += token
            return bindResult
        }
        override fun unbind(token: Long) { unbound += token; order += "unbind" }
        val order = mutableListOf<String>()
        override fun requestRefresh(token: Long, requestId: String, epochAtStart: Long): Boolean {
            requests += token to requestId
            return acceptRequests
        }
        override fun cancelRefresh(token: Long, requestId: String) {
            canceled += token to requestId
            order += "cancel"
        }
        override fun finish(token: Long) { finished += token }
        override fun discard(token: Long) { discarded += token }
    }

    @Test fun twoConsecutiveRunsKeepTheirOwnLeases() {
        val actions = FakeActions()
        val coordinator = CatalogRefreshJobCoordinator(actions)
        val tag1 = Any()
        assertTrue(coordinator.onStartJob(1L, tag1, scheduleEnabled = true, epochAtStart = 0L))
        coordinator.onServiceConnected(1L)
        val id1 = actions.requests.single().second
        coordinator.onCallback(1L, id1, ok = true, error = null)
        assertEquals(listOf(1L), actions.finished)
        assertEquals(listOf(1L), actions.unbound)

        val tag2 = Any()
        assertTrue(coordinator.onStartJob(2L, tag2, scheduleEnabled = true, epochAtStart = 0L))
        coordinator.onServiceConnected(2L)
        val id2 = actions.requests.last().second
        coordinator.onCallback(2L, id2, ok = false, error = "CATALOG_TIMEOUT")
        assertEquals(listOf(1L, 2L), actions.finished)
        assertEquals(listOf(1L, 2L), actions.unbound)
    }

    @Test fun lateCallbackOfAnOldRunCannotFinishTheNewRun() {
        val actions = FakeActions()
        val coordinator = CatalogRefreshJobCoordinator(actions)
        val tag1 = Any()
        assertTrue(coordinator.onStartJob(1L, tag1, true, 0L))
        coordinator.onServiceConnected(1L)
        val id1 = actions.requests.single().second
        coordinator.onStopJob(tag1) // releases run 1 without a finish
        assertEquals(emptyList<Long>(), actions.finished)

        assertTrue(coordinator.onStartJob(2L, Any(), true, 0L))
        coordinator.onServiceConnected(2L)
        val id2 = actions.requests.last().second
        coordinator.onCallback(1L, id1, ok = true, error = null) // stale token
        assertTrue("stale callback must not finish the new run", actions.finished.isEmpty())
        coordinator.onTimeout(1L) // stale timeout
        assertTrue(actions.finished.isEmpty())
        coordinator.onCallback(2L, id2, ok = true, error = null)
        assertEquals(listOf(2L), actions.finished)
    }

    @Test fun onStopJobWithStaleParamsIsIgnored() {
        val actions = FakeActions()
        val coordinator = CatalogRefreshJobCoordinator(actions)
        val tag1 = Any()
        coordinator.onStartJob(1L, tag1, true, 0L)
        coordinator.onServiceConnected(1L)
        val tag2 = Any()
        coordinator.onStartJob(2L, tag2, true, 0L)
        coordinator.onServiceConnected(2L)
        val requestId2 = actions.requests.last().second

        coordinator.onStopJob(tag1) // belongs to the abandoned run
        assertTrue(actions.canceled.none { it.second == requestId2 })
        assertFalse(actions.unbound.contains(2L))

        coordinator.onStopJob(tag2)
        assertTrue(actions.canceled.any { it.second == requestId2 })
        assertTrue(actions.unbound.contains(2L))
        assertTrue(actions.finished.isEmpty())
        // The request must be canceled through the retained binder before the lease is released.
        val cancelIndex = actions.order.indexOf("cancel")
        val unbindIndex = actions.order.indexOf("unbind")
        assertTrue(cancelIndex in 0 until unbindIndex)
    }

    @Test fun bindFailureReturnsFalseWithoutFinishing() {
        val actions = FakeActions().apply { bindResult = false }
        val coordinator = CatalogRefreshJobCoordinator(actions)
        assertFalse(coordinator.onStartJob(1L, Any(), scheduleEnabled = true, epochAtStart = 0L))
        assertTrue(actions.bound.isEmpty())
        assertTrue(actions.finished.isEmpty())
        assertTrue(actions.unbound.isEmpty())
    }

    @Test fun scheduleOffAtStartNeverBindsOrFinishes() {
        val actions = FakeActions()
        val coordinator = CatalogRefreshJobCoordinator(actions)
        assertFalse(coordinator.onStartJob(1L, Any(), scheduleEnabled = false, epochAtStart = 0L))
        assertTrue(actions.bound.isEmpty())
        assertTrue(actions.finished.isEmpty())
    }

    @Test fun rejectedRequestFinishesOnce() {
        val actions = FakeActions().apply { acceptRequests = false }
        val coordinator = CatalogRefreshJobCoordinator(actions)
        assertTrue(coordinator.onStartJob(1L, Any(), true, 0L))
        coordinator.onServiceConnected(1L)
        assertEquals(listOf(1L), actions.finished)
        assertEquals(listOf(1L), actions.unbound)
    }

    @Test fun timeoutCancelsTheRequestAndFinishesExactlyOnce() {
        val actions = FakeActions()
        val coordinator = CatalogRefreshJobCoordinator(actions)
        coordinator.onStartJob(1L, Any(), true, 0L)
        coordinator.onServiceConnected(1L)
        val id = actions.requests.single().second
        coordinator.onTimeout(1L)
        assertEquals(id, actions.canceled.single().second)
        assertEquals(listOf(1L), actions.finished)
        coordinator.onCallback(1L, id, ok = true, error = null)
        assertEquals(listOf(1L), actions.finished)
    }

    @Test fun scheduleOffMidRunFailsTheWaitingRequest() {
        val actions = FakeActions()
        val coordinator = CatalogRefreshJobCoordinator(actions)
        coordinator.onStartJob(1L, Any(), true, 0L)
        coordinator.onServiceConnected(1L)
        coordinator.onScheduleOff(1L)
        assertEquals(listOf(1L), actions.finished)
        assertTrue(actions.canceled.isNotEmpty())
    }
    @Test fun disconnectCancelsBeforeUnbindingAndDestroyDiscardsOnlyTheLiveRun() {
        val actions = FakeActions()
        val coordinator = CatalogRefreshJobCoordinator(actions)
        coordinator.onStartJob(1L, Any(), true, 0L)
        coordinator.onServiceConnected(1L)
        coordinator.onServiceLost(1L)
        assertEquals(listOf("cancel", "unbind"), actions.order)
        assertEquals(listOf(1L), actions.finished)
        coordinator.onStartJob(2L, Any(), true, 0L)
        coordinator.onServiceConnected(2L)
        coordinator.onStartJob(3L, Any(), true, 0L)
        assertEquals(listOf(2L), actions.discarded)
        coordinator.onDestroy()
        assertEquals(listOf(2L, 3L), actions.discarded)
        assertEquals(listOf(1L), actions.finished)
        assertEquals(listOf(1L, 2L, 3L), actions.unbound)
    }

}

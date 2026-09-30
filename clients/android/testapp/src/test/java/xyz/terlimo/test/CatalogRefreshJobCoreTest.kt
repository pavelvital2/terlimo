package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * §26.5 job-run state machine. Covers cold bound-only start, active-service start, reject,
 * disconnect, cycle result, timeout, Off and onStopJob, and proves jobFinished is taken at
 * most once and never after onStopJob.
 */
class CatalogRefreshJobCoreTest {

    @Test fun offScheduleEndsTheRunWithoutBinding() {
        val core = CatalogRefreshJobCore()
        assertFalse(core.start(scheduleEnabled = false))
        assertNull(core.takeFinish())
        assertNull(core.requestId())
    }

    @Test fun bindThenCompleteOK() {
        val core = CatalogRefreshJobCore()
        assertTrue(core.start(scheduleEnabled = true))
        val requestId = core.requestId()
        assertNotNull(requestId)
        assertTrue(core.onServiceConnected())
        core.onCycleResult(ok = true, error = null)
        val finish = core.takeFinish()
        assertNotNull(finish)
        assertTrue(finish!!.ok)
        assertNull(finish.failure)
        assertNull(core.takeFinish())
    }

    @Test fun rejectedRequestEndsTheRunWithRejected() {
        val core = CatalogRefreshJobCore()
        core.start(true)
        core.onServiceConnected()
        core.onRequestRejected()
        assertEquals(CatalogRefreshJobCore.Failure.REJECTED, core.takeFinish()!!.failure)
    }

    @Test fun bindFailureAndDisconnectAreDistinct() {
        val bound = CatalogRefreshJobCore(); bound.start(true); bound.onBindFailed()
        assertEquals(CatalogRefreshJobCore.Failure.BIND_FAILED, bound.takeFinish()!!.failure)
        val lost = CatalogRefreshJobCore(); lost.start(true); lost.onServiceConnected(); lost.onServiceDisconnected()
        assertEquals(CatalogRefreshJobCore.Failure.DISCONNECTED, lost.takeFinish()!!.failure)
    }

    @Test fun timeoutAndCycleErrorCarryTheirTokens() {
        val timed = CatalogRefreshJobCore(); timed.start(true); timed.onServiceConnected(); timed.onTimeout()
        assertEquals(CatalogRefreshJobCore.Failure.TIMEOUT, timed.takeFinish()!!.failure)
        val failed = CatalogRefreshJobCore(); failed.start(true); failed.onServiceConnected()
        failed.onCycleResult(ok = false, error = "CATALOG_TIMEOUT")
        val finish = failed.takeFinish()!!
        assertEquals(CatalogRefreshJobCore.Failure.CYCLE_ERROR, finish.failure)
        assertEquals("CATALOG_TIMEOUT", finish.error)
    }

    @Test fun scheduleOffMidRunFailsTheWaitingRequest() {
        val core = CatalogRefreshJobCore()
        core.start(true)
        core.onServiceConnected()
        core.onScheduleOff()
        assertEquals(CatalogRefreshJobCore.Failure.SCHEDULE_OFF, core.takeFinish()!!.failure)
    }

    @Test fun onStopJobReleasesTheLeaseWithoutAnyFinish() {
        val core = CatalogRefreshJobCore()
        core.start(true)
        core.onServiceConnected()
        core.onStopJob()
        assertNull(core.takeFinish())
        // A late callback after onStopJob must not produce a completion either.
        core.onCycleResult(ok = true, error = null)
        assertNull(core.takeFinish())
    }

    @Test fun finishIsTakenExactlyOnceEvenAfterLateEvents() {
        val core = CatalogRefreshJobCore()
        core.start(true)
        core.onServiceConnected()
        core.onCycleResult(ok = false, error = "VPN_STOPPED")
        assertNotNull(core.takeFinish())
        core.onTimeout()
        assertNull(core.takeFinish())
        assertNull(core.takeFinish())
    }

    @Test fun startIsIdempotentPerRun() {
        val core = CatalogRefreshJobCore()
        assertTrue(core.start(true))
        assertFalse(core.start(true))
    }
}

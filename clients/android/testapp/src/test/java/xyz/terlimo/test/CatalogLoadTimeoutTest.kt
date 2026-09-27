package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class CatalogLoadTimeoutTest {
    @Test fun unfinishedCatalogAttemptTimesOut() {
        val deadline = CatalogDeadlineLifecycle()
        deadline.arm("a")
        assertTrue(CatalogLoadTimeout.shouldStop("a", "a", deadline.pending("a")))
    }

    @Test fun acceptedCatalogThenLaterPhasesNeverStop() {
        val deadline = CatalogDeadlineLifecycle()
        deadline.arm("a")
        // Fresh catalogue accepted for the attempt: permanently disarmed.
        deadline.disarm("a")
        // Later NodeAuthenticating/ConfiguringVPN/readiness/switching callbacks must not stop.
        assertFalse(deadline.pending("a"))
        assertFalse(CatalogLoadTimeout.shouldStop("a", "a", deadline.pending("a")))
        assertFalse(CatalogLoadTimeout.shouldStop("a", "a", deadline.pending("a")))
    }

    @Test fun staleAttemptCallbackCannotStop() {
        val deadline = CatalogDeadlineLifecycle()
        deadline.arm("new")
        assertFalse(CatalogLoadTimeout.shouldStop("new", "old", deadline.pending("old")))
        assertFalse(CatalogLoadTimeout.shouldStop(null, "new", deadline.pending("new")))
    }

    @Test fun newAttemptReArmsOnlyItself() {
        val deadline = CatalogDeadlineLifecycle()
        deadline.arm("a")
        deadline.disarm("a")
        deadline.arm("b")
        assertFalse(CatalogLoadTimeout.shouldStop("b", "b", deadline.pending("a")))
        assertTrue(CatalogLoadTimeout.shouldStop("b", "b", deadline.pending("b")))
    }

    @Test fun catalogAcceptedBeforeDeadlineCallbackCannotResurrect() {
        val deadline = CatalogDeadlineLifecycle()
        // arm happens before child start; native accepts the catalogue immediately.
        deadline.arm("a")
        deadline.disarm("a")
        // The deadline callback fires later, during ConfiguringVPN.
        assertFalse(CatalogLoadTimeout.shouldStop("a", "a", deadline.pending("a")))
        // A stale disarm from a previous attempt cannot disarm the successor.
        deadline.arm("b")
        deadline.disarm("a")
        assertTrue(deadline.pending("b"))
    }

    @Test fun clearDisarmsUnconditionally() {
        val deadline = CatalogDeadlineLifecycle()
        deadline.arm("a")
        deadline.clear()
        assertFalse(deadline.pending("a"))
        assertFalse(CatalogLoadTimeout.shouldStop("a", "a", deadline.pending("a")))
    }
}

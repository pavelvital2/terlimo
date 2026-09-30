package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class ConnectDeadlineTest {
    @Test fun repeatConnectAndReadinessCannotRenewBudget() {
        val budget = ConnectDeadline()
        assertTrue(budget.start("first", 100))
        assertFalse(budget.start("first", 10_000))
        assertEquals(15_100, budget.deadline("first"))
        assertFalse(budget.expired("first", 15_099))
        assertTrue(budget.expired("first", 15_100))
        assertFalse(budget.complete("first", 15_100))
        assertFalse(budget.complete("first", 30_000))
    }

    @Test fun successDisarmsTimeoutAndStaleTimerCannotStopSuccessor() {
        val budget = ConnectDeadline()
        assertTrue(budget.start("first", 0))
        assertTrue(budget.complete("first", 14_999))
        assertFalse(budget.expired("first", 15_000))
        assertTrue(budget.start("second", 20_000))
        assertFalse(budget.expired("first", 40_000))
        assertEquals(35_000, budget.deadline("second"))
        budget.clear()
        assertFalse(budget.expired("second", 40_000))
    }

    // ─── Service adapter timer policy (virtual-time human wait, no 15s test sleep) ───

    @Test fun pausedBudgetSurvivesAHumanWaitLongerThanTheWholeBudget() {
        val deadline = ConnectDeadline()
        assertTrue(deadline.start("a1", now = 1_000L)) // 15s budget → end 16_000

        val paused = deadline.pause("a1", 1_000L)
        assertEquals(15_000L, paused)

        // Virtual 60s human wait: time advances, but the budget does not.
        val resumed = deadline.resume("a1", 61_000L)
        assertEquals(15_000L, resumed)
        assertFalse("not expired at the moment of resume", deadline.expired("a1", 61_000L))
        assertFalse(deadline.expired("a1", 75_999L))
        assertTrue(deadline.expired("a1", 76_000L))

        // The operation completes with the remaining budget.
        assertTrue("completes with the remaining budget", deadline.complete("a1", 74_000L))
        assertEquals(Long.MAX_VALUE, deadline.deadline("a1"))
    }

    @Test fun staleAttemptCanNotPauseResumeOrClearTheCurrentOne() {
        val deadline = ConnectDeadline()
        deadline.start("b1", 100_000L)
        assertNull("other attempt is not the owner", deadline.pause("a1", 100_000L))
        assertNull("resume without pause is a no-op", deadline.resume("b1", 100_000L))

        deadline.pause("b1", 100_000L)
        assertNull("second pause of the same attempt is a no-op", deadline.pause("b1", 101_000L))
        deadline.clear()
        assertNull("old attempt can not resume a new owner", deadline.resume("b1", 200_000L))
        assertEquals(Long.MAX_VALUE, deadline.deadline("b1"))

        deadline.start("c1", 300_000L)
        assertNull(deadline.resume("b1", 400_000L))
        assertEquals(315_000L, deadline.deadline("c1"))
        assertFalse(deadline.expired("c1", 314_999L))
        assertTrue(deadline.expired("c1", 315_000L))
    }

    @Test fun controllerPairsPauseAndWatchdogRearm() {
        val deadline = ConnectDeadline()
        var armed: Pair<String, Long>? = null
        var cancels = 0
        val controller = ConnectBudgetController(deadline, { cancels++ }, { attempt, delay -> armed = attempt to delay })

        deadline.start("a1", 0L)
        assertEquals(15_000L, controller.pause("a1", 0L))
        assertEquals("watchdog removed on pause", 1, cancels)

        assertEquals(15_000L, controller.resume("a1", 30_000L))
        assertEquals("a1" to 15_000L, armed)
        assertNull("second resume is a no-op", controller.resume("a1", 31_000L))
        assertEquals("a1" to 15_000L, armed)

        controller.pause("a1", 31_000L)
        assertEquals(2, cancels)
        deadline.clear()
        assertNull("stale resume can not re-arm", controller.resume("a1", 60_000L))
        assertEquals(2, cancels)
    }
}

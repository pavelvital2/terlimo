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
}

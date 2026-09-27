package xyz.terlimo.test

import java.net.SocketTimeoutException
import org.junit.Assert.*
import org.junit.Test

class ProbeBudgetTest {
    @Test fun eachOperationUsesOnlyRemainingBudget() {
        var clock = 0L
        val budget = ProbeBudget(15_000) { clock }
        assertEquals(4000, budget.timeoutMs())
        clock = 14_999
        assertEquals(1, budget.timeoutMs()) // never0 (which means unbounded socket timeout)
        clock = 15_000
        assertTrue(budget.expired)
        try { budget.timeoutMs(); fail("deadline extended") } catch (_: SocketTimeoutException) { }
        clock = 20_000
        try { budget.timeoutMs(); fail("late result permitted") } catch (_: SocketTimeoutException) { }
    }
}

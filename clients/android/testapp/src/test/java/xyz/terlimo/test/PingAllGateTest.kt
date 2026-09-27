package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class PingAllGateTest {
    @Test
    fun `explicit start sequences snapshot order once`() {
        val step = PingAllGate.start(PingAllState(), listOf("a", "b", "c"))
        assertTrue(step.state.active)
        assertEquals(listOf("a", "b", "c"), step.state.queue)
        assertEquals("a", step.startId)
        assertEquals("a", step.state.expectedId)
        assertFalse(step.state.sorted)
    }

    @Test
    fun `results advance then finish sorted`() {
        var state = PingAllGate.start(PingAllState(), listOf("a", "b")).state
        val r1 = PingAllGate.onResult(state, "a")
        state = r1.state
        assertEquals("b", r1.startId)
        val r2 = PingAllGate.onResult(state, "b")
        assertNull(r2.startId)
        assertFalse(r2.state.active)
        assertTrue(r2.state.sorted)
    }

    @Test
    fun `late and foreign results never advance the run`() {
        val state = PingAllGate.start(PingAllState(), listOf("a", "b")).state
        val foreign = PingAllGate.onResult(state, "zzz")
        assertTrue(foreign.ignore)
        assertEquals("a", foreign.state.expectedId)
        val afterCancel = PingAllGate.onResult(PingAllGate.cancel(state), "a")
        assertTrue(afterCancel.ignore)
        assertFalse(afterCancel.state.sorted)
    }

    @Test
    fun `cancel clears running states and drops late results`() {
        val pings = mapOf(
            "a" to NodePingState.Running(1L, 0L, 12_000L, "p-1-1-a"),
            "b" to NodePingState.Idle,
            "c" to NodePingState.Success(50L),
        )
        val cancelled = PingAllGate.cancelPings(pings)
        assertTrue(cancelled["a"] is NodePingState.Cancelled)
        assertTrue(cancelled["b"] is NodePingState.Idle)
        assertTrue(cancelled["c"] is NodePingState.Success)
        assertTrue(PingAllGate.isLateAfterCancel(cancelled, "a"))
        assertFalse(PingAllGate.isLateAfterCancel(cancelled, "b"))
    }

    @Test
    fun `rapid cancel then restart ignores a late old result for the same node`() {
        val first = PingAllGate.start(PingAllState(), listOf("n")).state
        val cancelled = PingAllGate.cancel(first)
        val second = PingAllGate.start(cancelled, listOf("n"))
        assertTrue(second.state.active)
        assertEquals("n", second.state.expectedId)
        // The old run's late result is fenced by the native probeEpoch and, host-side, by the
        // cancelled state: a stale frame must never advance the restarted run.
        assertTrue(PingAllGate.isLateAfterCancel(PingAllGate.cancelPings(
            mapOf("n" to NodePingState.Running(1L, 0L, 12_000L, "p-1-1-a"))), "n"))
        val applied = PingAllGate.onResult(second.state, "n")
        assertFalse(applied.state.active)
        assertTrue(applied.state.sorted)
    }

    @Test
    fun `cancel restores grouped view and empty start ignores`() {
        val state = PingAllGate.start(PingAllState(), listOf("a", "b")).state
        val cancelled = PingAllGate.cancel(state)
        assertFalse(cancelled.active)
        assertFalse(cancelled.sorted)
        assertNull(cancelled.expectedId)
        assertTrue(cancelled.queue.isEmpty())
        val empty = PingAllGate.start(PingAllState(), emptyList())
        assertTrue(empty.ignore)
        assertNull(empty.startId)
    }
}

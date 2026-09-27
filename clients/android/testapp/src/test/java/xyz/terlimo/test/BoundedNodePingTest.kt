package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class BoundedNodePingTest {
    private fun id(seq: Int) = "p-1-$seq-a"

    @Test fun sameNodeStartsOnlyOnceAndCapacityIsBounded() {
        val ping = BoundedNodePing(listOf("a", "b", "c"), maxConcurrent = 2)
        assertTrue(ping.start("a", id(1), 0) is PingStartResult.Started)
        assertEquals(PingStartResult.AlreadyRunning, ping.start("a", id(2), 1))
        assertTrue(ping.start("b", id(2), 1) is PingStartResult.Started)
        assertEquals(PingStartResult.CapacityReached, ping.start("c", id(3), 2))
        assertEquals(2, ping.snapshot().values.count { it is NodePingState.Running })
        assertEquals(id(1), (ping.snapshot()["a"] as NodePingState.Running).probeId)
    }

    @Test fun timeoutChangesOnlyThatNodeAndNeverSelectsOrStartsAnother() {
        val ping = BoundedNodePing(listOf("a", "b"), timeoutMs = 500)
        ping.start("a", id(1), 100)
        assertEquals(setOf("a"), ping.expire(600))
        assertEquals(NodePingState.Timeout, ping.snapshot()["a"])
        assertEquals(NodePingState.Idle, ping.snapshot()["b"])
    }

    @Test fun cancellationIsTerminalAndLateCompletionIsIgnored() {
        val ping = BoundedNodePing(listOf("a"))
        val token = (ping.start("a", id(1), 0) as PingStartResult.Started).token
        assertTrue(ping.cancel("a", token))
        assertFalse(ping.complete("a", token, 10))
        assertEquals(NodePingState.Cancelled, ping.snapshot()["a"])
    }

    @Test fun staleTokenCannotCompleteReplacementRun() {
        val ping = BoundedNodePing(listOf("a"))
        val old = (ping.start("a", id(1), 0) as PingStartResult.Started).token
        ping.cancel("a", old)
        val current = (ping.start("a", id(2), 20) as PingStartResult.Started).token
        assertFalse(ping.complete("a", old, 30))
        assertTrue(ping.complete("a", current, 45))
        // A locally timed completion carries only the echo RTT; no setup diagnostic exists.
        assertEquals(NodePingState.Success(25, null), ping.snapshot()["a"])
    }

    @Test fun unknownNodeNeverCreatesStateOrWork() {
        val ping = BoundedNodePing(listOf("a"))
        assertEquals(PingStartResult.UnknownNode, ping.start("b", id(1), 0))
        assertEquals(setOf("a"), ping.snapshot().keys)
    }

    @Test fun malformedProbeIdIsNeverAccepted() {
        val ping = BoundedNodePing(listOf("a"))
        listOf("", "x".repeat(65), "bad id", "probe:id").forEach {
            assertThrows(IllegalArgumentException::class.java) { ping.start("a", it, 0) }
        }
        assertEquals(NodePingState.Idle, ping.snapshot()["a"])
    }

    @Test fun constructorBoundsAreStrict() {
        assertThrows(IllegalArgumentException::class.java) { BoundedNodePing(listOf("a"), 0) }
        assertThrows(IllegalArgumentException::class.java) { BoundedNodePing(listOf("a"), timeoutMs = 31_000) }
        assertThrows(IllegalArgumentException::class.java) { BoundedNodePing(listOf("a", "a")) }
    }
}

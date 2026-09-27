package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

/**
 * S5 07.2 deterministic interleaving tests through the actual ProbeFrameHandler the
 * service delegates to: node/probeId/attempt/runtime fences, single completion,
 * busy retention, legacy boundary and the no-fake-RTT rule.
 */
class ProbeFrameHandlerTest {
    private val attempt = "attempt-a"
    private val otherAttempt = "attempt-b"

    private fun running(id: String, token: Long = 1L, probeId: String = "p-1-1-a") =
        NodePingState.Running(token, 0L, 12_000L, probeId)

    private fun id(nodeSeq: Int, suffix: String = "a") = "p-1-$nodeSeq-$suffix"

    private fun apply(pings: Map<String, NodePingState>, decision: ProbeFrameDecision): Map<String, NodePingState> =
        if (decision is ProbeFrameDecision.Settle) pings + (decision.nodeId to decision.state) else pings

    @Test
    fun `cancelled run can never settle the restarted run and completion happens once`() {
        val frames = ProbeFrameHandler()
        frames.begin(ProbeFence("a", id(1), attempt, 0L))
        var pings: Map<String, NodePingState> = mapOf("a" to running(id(1)).copy(token = 1))
        assertEquals(ProbeFence("a", id(1), attempt, 0L), frames.cancel("a"))
        pings = pings + ("a" to NodePingState.Cancelled)
        assertFalse(frames.isActive("a"))

        frames.begin(ProbeFence("a", id(2), attempt, 0L))
        pings = pings + ("a" to running(id(2)).copy(token = 2))
        assertTrue(frames.isActive("a"))

        // The old run's queued result is stale by probe_id and changes nothing.
        assertEquals(ProbeFrameDecision.Drop,
            frames.onFrame("a", id(1), "ok", 50L, null, attempt, 0L))
        assertEquals(running(id(2)).copy(token = 2), pings["a"])

        val settled = frames.onFrame("a", id(2), "ok", 77L, null, attempt, 0L)
        pings = apply(pings, settled)
        assertEquals(NodePingState.Success(77L), pings["a"])
        assertFalse(frames.isActive("a"))

        // A duplicate of the completing frame is ignored exactly once.
        assertEquals(ProbeFrameDecision.Drop,
            frames.onFrame("a", id(2), "ok", 99L, null, attempt, 0L))
        assertEquals(NodePingState.Success(77L), pings["a"])
    }

    @Test
    fun `cancel retires only its own node and a frame after cancel is dropped`() {
        val frames = ProbeFrameHandler()
        frames.begin(ProbeFence("a", id(1), attempt, 0L))
        assertNull(frames.cancel("b"))
        assertTrue(frames.isActive("a"))
        assertNotNull(frames.cancel("a"))
        assertEquals(ProbeFrameDecision.Drop,
            frames.onFrame("a", id(1), "ok", 10L, null, attempt, 0L))
    }

    @Test
    fun `missing malformed and foreign ids on an id-bearing request are dropped`() {
        val frames = ProbeFrameHandler()
        frames.begin(ProbeFence("a", id(1), attempt, 0L))
        assertFalse(ProbeIds.isValid("bad id"))
        listOf<() -> ProbeFrameDecision>(
            { frames.onFrame("a", null, "ok", 50L, null, attempt, 0L) },
            { frames.onFrame("a", "bad id", "ok", 50L, null, attempt, 0L) },
            { frames.onFrame("a", "x".repeat(65), "ok", 50L, null, attempt, 0L) },
            { frames.onFrame("a", id(2), "ok", 50L, null, attempt, 0L) },
        ).forEach { assertEquals(ProbeFrameDecision.Drop, it()) }
        assertTrue(frames.isActive("a"))
    }

    @Test
    fun `node attempt and runtime mismatches are dropped`() {
        val frames = ProbeFrameHandler()
        frames.begin(ProbeFence("a", id(1), attempt, 7L))
        assertEquals(ProbeFrameDecision.Drop,
            frames.onFrame("b", id(1), "ok", 50L, null, attempt, 7L))
        assertEquals(ProbeFrameDecision.Drop,
            frames.onFrame("a", id(1), "ok", 50L, null, otherAttempt, 7L))
        assertEquals(ProbeFrameDecision.Drop,
            frames.onFrame("a", id(1), "ok", 50L, null, attempt, 8L))
        assertTrue(frames.isActive("a"))
        assertTrue(frames.onFrame("a", id(1), "ok", 50L, null, attempt, 7L) is ProbeFrameDecision.Settle)
    }

    @Test
    fun `ok without a truthful rtt fails closed instead of faking a success`() {
        listOf(
            Triple("missing", null as Long?, null as Long?),
            Triple("negative", -1L, null as Long?),
            Triple("over budget", 12_001L, null as Long?),
            Triple("setup only", null as Long?, 900L),
        ).forEach { (label, rtt, setup) ->
            val frames = ProbeFrameHandler()
            frames.begin(ProbeFence("a", id(1), attempt, 0L))
            assertEquals(label, ProbeFrameDecision.Settle("a", NodePingState.Failed),
                frames.onFrame("a", id(1), "ok", rtt, setup, attempt, 0L))
            assertFalse(frames.isActive("a"))
        }
    }

    @Test
    fun `busy with a matching id retains the running request and truthful statuses settle`() {
        val frames = ProbeFrameHandler()
        frames.begin(ProbeFence("a", id(1), attempt, 0L))
        assertEquals(ProbeFrameDecision.KeepRunning,
            frames.onFrame("a", id(1), "busy", null, null, attempt, 0L))
        assertTrue(frames.isActive("a"))
        assertEquals(ProbeFrameDecision.Settle("a", NodePingState.Timeout),
            frames.onFrame("a", id(1), "timeout", null, null, attempt, 0L))
        assertFalse(frames.isActive("a"))

        val failed = ProbeFrameHandler()
        failed.begin(ProbeFence("a", id(1), attempt, 0L))
        assertEquals(ProbeFrameDecision.Settle("a", NodePingState.Failed),
            failed.onFrame("a", id(1), "failed", null, null, attempt, 0L))

        val unknown = ProbeFrameHandler()
        unknown.begin(ProbeFence("a", id(1), attempt, 0L))
        assertEquals(ProbeFrameDecision.Drop,
            unknown.onFrame("a", id(1), "weird", null, null, attempt, 0L))
        assertTrue(unknown.isActive("a"))
    }

    @Test
    fun `legacy request without an id accepts only a legacy result`() {
        val frames = ProbeFrameHandler()
        frames.begin(ProbeFence("a", null, attempt, 0L))
        assertEquals(ProbeFrameDecision.Drop,
            frames.onFrame("a", id(1), "ok", 50L, null, attempt, 0L))
        assertEquals(ProbeFrameDecision.Settle("a", NodePingState.Success(11L)),
            frames.onFrame("a", null, "ok", 11L, null, attempt, 0L))

        val idBearing = ProbeFrameHandler()
        idBearing.begin(ProbeFence("a", id(1), attempt, 0L))
        assertEquals(ProbeFrameDecision.Drop,
            idBearing.onFrame("a", null, "ok", 50L, null, attempt, 0L))
    }

    @Test
    fun `direct fallback keeps rtt and setup distinct and displays only the rtt`() {
        val frames = ProbeFrameHandler()
        frames.begin(ProbeFence("a", id(1), attempt, 0L))
        val decision = frames.onFrame("a", id(1), "ok", 40L, 900L, attempt, 0L)
        assertEquals(ProbeFrameDecision.Settle("a", NodePingState.Success(40L, 900L)), decision)
        val cards = CatalogCardProjection.project(
            listOf(CatalogCardInput("a", "Москва", "Россия")), "a",
            mapOf("a" to (decision as ProbeFrameDecision.Settle).state))
        assertEquals(40L, cards.cards[0].rttMs)
        assertFalse(cards.cards[0].stateText.contains("900"))
    }

    @Test
    fun `unexpected ok setup value is never shown and never replaces a truthful rtt`() {
        val frames = ProbeFrameHandler()
        frames.begin(ProbeFence("a", id(1), attempt, 0L))
        val decision = frames.onFrame("a", id(1), "ok", 40L, 12_001L, attempt, 0L)
        assertEquals(ProbeFrameDecision.Settle("a", NodePingState.Success(40L, null)), decision)
    }
}

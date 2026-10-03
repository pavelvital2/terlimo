package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class CatalogProbeAdmissionTest {
    @Test fun batchUsesCredentialIdsAndRefreshRetiresRunningAttribution() {
        val nodes = listOf(NodeLabel("A", "A"), NodeLabel("B", "B"))
        val state = ViewState(phase = "CatalogReady", nodes = nodes, selectedNodeId = "A")
        // No readiness map is an input to the production queue admission function.
        val batch = PingAllGate.start(state.pingAll, PingAllGate.catalogIds(state))
        assertEquals(listOf("A", "B"), batch.state.queue)
        assertEquals("A", state.selectedNodeId)
        assertTrue(PingAllGate.catalogIds(state.copy(displayMode = CatalogDisplayMode.BROWSE)).isEmpty())
        assertTrue(PingAllGate.catalogIds(state.copy(phase = "Idle")).isEmpty())
        val frames = ProbeFrameHandler()
        frames.begin(ProbeFence("B", "manual-1", "attempt", 1))
        val running = mapOf("B" to NodePingState.Running(1, 1, 12001, "manual-1"))
        val retired = PingAllGate.retirePings(running, frames)
        assertEquals(NodePingState.Cancelled, retired["B"])
        assertFalse(frames.isActive("B"))
        assertEquals(ProbeFrameDecision.Drop,
            frames.onFrame("B", "manual-1", "ok", 42, null, "attempt", 1))
        assertFalse(PingAllGate.reset().active)
    }
}

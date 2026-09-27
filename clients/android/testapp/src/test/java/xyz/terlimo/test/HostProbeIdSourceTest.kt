package xyz.terlimo.test

import java.io.File
import org.junit.Assert.*
import org.junit.Test

/**
 * S5 07.2 host side of the local bridge contract: bounded unique probe_id generation,
 * sequential ping-all ordering, the probe_id carried in probe_node and the delegation of
 * node_probe_result to ProbeFrameHandler. No device, native or network action.
 */
class HostProbeIdSourceTest {
    private fun source(path: String): String =
        listOf(File(path), File("testapp/$path"), File("../$path")).first { it.isFile }.readText()

    private val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
    private val catalogCards = source("src/main/java/xyz/terlimo/test/CatalogCards.kt")
    private val catalogView = source("src/main/java/xyz/terlimo/test/ServerCatalogView.kt")

    @Test
    fun `probe ids are valid unique and deterministic with injected entropy`() {
        val ids = ProbeIds { 0x0F1EL }
        val first = ids.nextManual()
        val second = ids.nextManual()
        assertEquals("p-1-1-f1e", first)
        assertEquals("p-2-1-f1e", second)
        assertNotEquals(first, second)
        assertEquals("p-1-1-ffffffffffffffff", ProbeIds.format(1L, 1L, -1L))
        listOf(first, second, ProbeIds.format(Long.MAX_VALUE, Long.MAX_VALUE, Long.MIN_VALUE)).forEach {
            assertTrue(it, ProbeIds.isValid(it))
            assertTrue(it.length <= 64)
        }
        listOf("", "x".repeat(65), "p 1", "p/1", "ю").forEach { assertFalse(it, ProbeIds.isValid(it)) }
    }

    @Test
    fun `manual and ping-all runs issue unique ids even with colliding entropy`() {
        val ids = ProbeIds { 7L }
        val manual = ids.nextManual()
        val plan = ProbeRunPlan(listOf("a", "b"), ids)
        val first = plan.next()
        plan.advance()
        val second = plan.next()
        val nextManual = ids.nextManual()
        listOf(manual, first.probeId, second.probeId, nextManual).forEach { assertTrue(it, ProbeIds.isValid(it)) }
        assertEquals(4, setOf(manual, first.probeId, second.probeId, nextManual).size)
    }

    @Test
    fun `ping-all plan keeps snapshot order one active request and no overlap`() {
        val ids = ProbeIds { 1L }
        val plan = ProbeRunPlan(listOf("c", "a", "b"), ids)
        assertNull(plan.activeNodeId)
        assertEquals("c", plan.next().nodeId)
        assertEquals("c", plan.activeNodeId)
        assertThrows(IllegalStateException::class.java) { plan.next() }
        plan.advance()
        assertEquals("a", plan.next().nodeId)
        assertThrows(IllegalStateException::class.java) { plan.next() }
        plan.advance()
        assertEquals("b", plan.next().nodeId)
        plan.advance()
        assertTrue(plan.done)
        assertThrows(IllegalStateException::class.java) { plan.next() }
        assertThrows(IllegalArgumentException::class.java) { ProbeRunPlan(listOf("a", "a"), ProbeIds { 1L }) }
        assertThrows(IllegalArgumentException::class.java) { ProbeRunPlan(emptyList(), ProbeIds { 1L }) }
    }

    @Test
    fun `service sends a fresh probe id per request and retires it on cancel`() {
        val probe = service.substringAfter("\"probe\" ->").substringBefore("\"telegram_register\" ->")
        assertTrue(probe.contains("probeIds.nextManual()"))
        assertTrue(probe.contains("probeFrames.begin(ProbeFence(id, probeId, attempt, activeRuntimeEpoch))"))
        assertTrue(probe.contains(".put(\"probe_id\", probeId)"))
        assertTrue(probe.contains("probeFrames.cancel(id)"))
        // Cancel retires the old run and publishes the truthful terminal display state.
        assertTrue(probe.contains("pings = view.pings + (id to NodePingState.Cancelled)"))
        assertFalse(probe.substringBefore("probeFrames.cancel(id)").contains("probeFrames.begin("))
    }

    @Test
    fun `service delegates results to the handler with the extended optional key set`() {
        val result = service.substringAfter("\"node_probe_result\" ->").substringBefore("\"switch_result\" ->")
        assertTrue(result.contains("probeFrames.onFrame("))
        assertTrue(result.contains("setOf(\"probe_id\", \"rtt_ms\", \"transport_setup_ms\")"))
        assertTrue(result.contains("if (probeId != null && !ProbeIds.isValid(probeId)) return"))
        assertTrue(result.contains("(event.opt(\"rtt_ms\") as? Number)?.toLong()"))
        assertTrue(result.contains("(event.opt(\"transport_setup_ms\") as? Number)?.toLong()"))
        assertTrue(result.contains("decision is ProbeFrameDecision.Settle"))
        assertFalse(result.contains("NodePingState.Success(event.getLong(\"transport_setup_ms\")"))
        assertFalse(result.contains("check(view.nodes.any { it.id == id })"))
    }

    @Test
    fun `display uses only the echo rtt and never the setup diagnostic`() {
        assertTrue(catalogCards.contains("ping.rttMs"))
        assertFalse(catalogCards.contains("ping.latencyMs"))
        assertTrue(catalogView.contains("ping.rttMs"))
        assertFalse(catalogView.contains("ping.latencyMs"))
        assertFalse(catalogCards.contains("setupMs"))
        assertFalse(catalogView.contains("setupMs"))
    }
}

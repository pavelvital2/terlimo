package xyz.terlimo.test

import java.io.File
import org.junit.Assert.*
import org.junit.Test

class SwitchErrorCatalogTest {
    private val nodes = listOf(NodeLabel("A", "A"), NodeLabel("B", "B"))

    @Test fun readyCatalogStateCannotInvalidateSuccessfulAToB() = readySequence("A", "B")
    @Test fun readyCatalogStateCannotInvalidateSuccessfulBToA() = readySequence("B", "A")

    private fun readySequence(active: String, target: String) {
        val switching = ViewState("SwitchingServer", nodes = nodes,
            selectedNodeId = active, pendingNodeId = target,
            pendingSwitchId = "operation", pendingSwitchRevision = "44", catalogRevision = "44")
        // Actual native success order after vpn_result readiness:
        // chooseNode(target), publishCatalog -> catalog + state(CatalogReady), switch_result(ok).
        val catalog = NodeSelection.applyCatalog(switching, NodeCatalog(nodes, target, "44"), null)
        val phase = NodeSelection.applyNativePhase(catalog, "CatalogReady")
        assertEquals("SwitchingServer", phase.phase)
        assertEquals(active, phase.selectedNodeId)
        val connected = ActiveNodeSwitch.success(phase, target, target, "operation", "44")
        assertEquals("Connected", connected.phase)
        assertEquals(target, connected.selectedNodeId)
        assertNull(connected.pendingSwitchId)
        assertNull(connected.error)
    }

    @Test fun rejectedTargetIsNotSilencedByFollowingCatalogInEitherDirection() {
        for ((active, target) in listOf("A" to "B", "B" to "A")) {
            for (code in listOf("BAD_CATALOG", "NODE_UNAVAILABLE")) {
                // BAD_CATALOG is the native ValidateNode result for an expired,
                // nonrevoked target. NODE_UNAVAILABLE also covers fresh targets.
                val connected = ViewState("Connected", nodes = nodes,
                    selectedNodeId = active, catalogRevision = "44")
                assertTrue(ActiveNodeSwitch.canStart(connected, target))
                // Exact pre-cutover switch_result(failed, rollback_allowed=true)
                // transition in SessionService; no active ownership is removed.
                val failed = connected.copy(error = code)
                val afterCatalog = NodeSelection.applyCatalog(failed,
                    NodeCatalog(nodes, active, "44"), null)
                assertEquals("Connected", afterCatalog.phase)
                assertEquals(active, afterCatalog.selectedNodeId)
                assertNull(afterCatalog.pendingNodeId)
                assertEquals("failure must survive metadata publication", code, afterCatalog.error)
                assertFalse(UserStatusText.error(afterCatalog.error!!).isNullOrBlank())
            }
        }
    }

    @Test fun catalogStillPreservesPendingCutoverAndClearsOldResumeError() {
        val pending = ViewState("SwitchingServer", nodes = nodes, selectedNodeId = "A",
            pendingNodeId = "B", pendingSwitchId = "operation", pendingSwitchRevision = "44")
        val updated = NodeSelection.applyCatalog(pending, NodeCatalog(nodes, "B", "45"), null)
        assertEquals("A", updated.selectedNodeId)
        assertEquals("B", updated.pendingNodeId)
        assertEquals("operation", updated.pendingSwitchId)
        assertEquals("44", updated.pendingSwitchRevision)
        val resumed = NodeSelection.applyCatalog(ViewState("Error", error = "CATALOG_TIMEOUT"),
            NodeCatalog(nodes, "A", "45"), null)
        assertEquals("CatalogReady", resumed.phase)
        assertNull(resumed.error)
    }

    @Test fun connectedCatalogRendersTheRetainedError() {
        val path = "src/main/java/xyz/terlimo/test/ServerCatalogView.kt"
        val source = listOf(File(path), File("testapp/$path")).first { it.isFile }.readText()
        val content = source.substringAfter("private fun addContent(state: ViewState)")
            .substringBefore("private fun addSelected")
        assertTrue(content.contains("state.error"))
        assertTrue(content.contains("addError("))
    }
}

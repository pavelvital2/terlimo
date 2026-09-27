package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class ActiveNodeSwitchTest {
    private val nodes = listOf(NodeLabel("A", "TEST1", ""), NodeLabel("B", "TEST2", ""))

    @Test fun onlyExplicitDifferentCatalogNodeStarts() {
        val connected = ViewState("Connected", nodes = nodes, selectedNodeId = "A", catalogRevision = "14")
        assertTrue(ActiveNodeSwitch.canStart(connected, "B"))
        assertFalse(ActiveNodeSwitch.canStart(connected, "A"))
        assertFalse(ActiveNodeSwitch.canStart(connected, "missing"))
        assertFalse(ActiveNodeSwitch.canStart(connected.copy(phase = "CatalogReady"), "B"))
        assertFalse(ActiveNodeSwitch.canStart(connected.copy(pendingNodeId = "B"), "B"))
        assertFalse(ActiveNodeSwitch.canStart(connected.copy(catalogRevision = ""), "B"))
    }

    @Test fun staleCompletionCannotReplaceLastGood() {
        val operation = "11111111-1111-1111-1111-111111111111"
        val switching = ViewState("SwitchingServer", nodes = nodes, selectedNodeId = "A", pendingNodeId = "B",
            catalogRevision = "14", pendingSwitchId = operation, pendingSwitchRevision = "14")
        assertEquals("B", ActiveNodeSwitch.success(switching, "B", "B", operation, "14").selectedNodeId)
        assertEquals("A", ActiveNodeSwitch.failure(switching, "A", "B", operation, "14", "NODE_UNAVAILABLE").selectedNodeId)
        listOf(
            { ActiveNodeSwitch.success(switching, "A", "B", operation, "14") },
            { ActiveNodeSwitch.success(switching, "B", "B", "22222222-2222-2222-2222-222222222222", "14") },
            { ActiveNodeSwitch.success(switching, "B", "B", operation, "15") },
            { ActiveNodeSwitch.failure(switching, "A", "A", operation, "14", "NODE_UNAVAILABLE") },
        ).forEach { action -> assertEquals("STALE_SWITCH_COMPLETION", runCatching(action).exceptionOrNull()?.message) }
    }
}

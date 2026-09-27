package xyz.terlimo.test

import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

class NodeSelectionTest {
    @Test
    fun selectionIsPreservedByIdAcrossReorder() {
        val first = listOf(NodeLabel("a", "A"), NodeLabel("b", "B"))
        val reordered = listOf(NodeLabel("b", "B"), NodeLabel("a", "A"))

        assertEquals("b", NodeSelection.displayedNodeId(first, "b"))
        assertEquals(1, NodeSelection.spinnerPosition(reordered, "b"))
        assertEquals("b", NodeSelection.nodeIdAtSpinnerPosition(reordered, 1))
    }

    @Test
    fun removedSavedSelectionDoesNotFallBack() {
        val remaining = listOf(NodeLabel("b", "B"))

        assertNull(NodeSelection.displayedNodeId(remaining, "a"))
        assertNull(NodeSelection.connectableNodeId(remaining, "a"))
        assertEquals(0, NodeSelection.spinnerPosition(remaining, "a"))
    }

    @Test
    fun emptySelectionNeverDefaultsEvenForSoleNode() {
        assertNull(NodeSelection.displayedNodeId(listOf(NodeLabel("a", "A")), ""))
        assertNull(NodeSelection.displayedNodeId(listOf(NodeLabel("a", "A"), NodeLabel("b", "B")), ""))
    }

    @Test
    fun duplicateNodeIdsAreRejected() {
        val event = JSONObject(
            """{"nodes":[{"node_id":"same","name":"A","country_code":"RU"},{"node_id":"same","name":"B","country_code":"RU"}],"selected_node_id":"same"}"""
        )

        assertThrows(IllegalStateException::class.java) { NodeSelection.parseCatalog(event) }
    }

    @Test fun countryIsPublicDisplayMetadataButMustKeepWireShape() {
        val event = JSONObject("""{"nodes":[{"node_id":"a","name":"A","country_code":""}],"selected_node_id":""}""")
        assertEquals("", NodeSelection.parseCatalog(event).nodes.single().countryCode)
        for (invalid in listOf(
            """{"nodes":[{"node_id":"a","name":"A"}],"selected_node_id":""}""",
            """{"nodes":[{"node_id":"a","name":"A","country_code":"R"}],"selected_node_id":""}""",
        )) assertThrows(IllegalStateException::class.java) { NodeSelection.parseCatalog(JSONObject(invalid)) }
    }

    private fun ids(count: Int): List<String> =
        (0 until count).map { "gw-" + it.toString().padStart(4, '0') }

    private fun catalogEvent(nodeIds: List<String>, selected: String, revision: String = "rev-${nodeIds.size}"): JSONObject {
        val entries = JSONArray()
        nodeIds.forEach { id ->
            val index = id.removePrefix("gw-").toInt()
            entries.put(JSONObject()
                .put("node_id", id)
                .put("name", "Server " + index.toString().padStart(4, '0'))
                .put("country_code", if (index % 2 == 0) "DE" else ""))
        }
        return JSONObject().put("nodes", entries).put("selected_node_id", selected).put("revision", revision)
    }

    @Test fun threeNodeCatalogParsesFullyWithSelectionAtFirstMiddleAndLast() {
        val nodeIds = listOf("gw-a", "gw-b", "gw-c")
        val nodes = JSONArray()
        nodeIds.forEach { nodes.put(JSONObject().put("node_id", it).put("name", it).put("country_code", "")) }
        for (selected in nodeIds) {
            val catalog = NodeSelection.parseCatalog(
                JSONObject().put("nodes", nodes).put("selected_node_id", selected).put("revision", "rev-3"))
            assertEquals(nodeIds, catalog.nodes.map { it.id })
            assertEquals(selected, catalog.selectedNodeId)
            assertEquals("rev-3", catalog.revision)
        }
    }

    @Test fun thousandNodeCatalogFitsBridgeFrameBudgetAndParsesWithoutTruncation() {
        val nodeIds = ids(1000)
        val event = catalogEvent(nodeIds, "gw-0500")
        assertTrue(event.toString().toByteArray(Charsets.UTF_8).size < NativeProcess.MAX_LINE)

        val catalog = NodeSelection.parseCatalog(event)
        assertEquals(1000, catalog.nodes.size)
        assertEquals(nodeIds, catalog.nodes.map { it.id })
        assertEquals("Server 0000", catalog.nodes.first().name)
        assertEquals("Server 0999", catalog.nodes.last().name)
        assertEquals("gw-0500", catalog.selectedNodeId)
        assertEquals("rev-1000", catalog.revision)
    }

    @Test fun emptyNodeListIsRejected() {
        assertThrows(IllegalStateException::class.java) { NodeSelection.parseCatalog(catalogEvent(emptyList(), "")) }
    }

    @Test fun entryGuardsHoldWithoutTheTwoNodeCap() {
        val longName = "N".repeat(150)
        val three = JSONObject("""{"nodes":[
            {"node_id":"a","name":"$longName","country_code":"ru"},
            {"node_id":"b","name":"B","country_code":""},
            {"node_id":"c","name":"C","country_code":"DE"}],"selected_node_id":"c"}""")
        val parsed = NodeSelection.parseCatalog(three)
        assertEquals(listOf("a", "b", "c"), parsed.nodes.map { it.id })
        assertEquals(100, parsed.nodes.first().name.length)
        assertEquals("RU", parsed.nodes.first().countryCode)

        val duplicate = JSONObject("""{"nodes":[
            {"node_id":"a","name":"A","country_code":""},
            {"node_id":"b","name":"B","country_code":""},
            {"node_id":"a","name":"C","country_code":""}],"selected_node_id":"a"}""")
        assertThrows(IllegalStateException::class.java) { NodeSelection.parseCatalog(duplicate) }

        val badCountry = JSONObject("""{"nodes":[
            {"node_id":"a","name":"A","country_code":""},
            {"node_id":"b","name":"B","country_code":"RUS"},
            {"node_id":"c","name":"C","country_code":""}],"selected_node_id":"a"}""")
        assertThrows(IllegalStateException::class.java) { NodeSelection.parseCatalog(badCountry) }

        val missingName = JSONObject("""{"nodes":[
            {"node_id":"a","name":"A","country_code":""},
            {"node_id":"b","country_code":""},
            {"node_id":"c","name":"C","country_code":""}],"selected_node_id":"a"}""")
        assertThrows(IllegalStateException::class.java) { NodeSelection.parseCatalog(missingName) }
    }

    @Test fun selectionAbsentFromCatalogIsRejectedForDisplayAndConnect() {
        val catalog = NodeSelection.parseCatalog(catalogEvent(ids(3), "gw-9999"))
        assertNull(NodeSelection.displayedNodeId(catalog.nodes, catalog.selectedNodeId))
        assertNull(NodeSelection.connectableNodeId(catalog.nodes, catalog.selectedNodeId))
        assertNull(RetainedCatalogPolicy.connectableId("Idle", catalog.nodes, catalog.selectedNodeId))
        assertEquals(0, NodeSelection.spinnerPosition(catalog.nodes, catalog.selectedNodeId))
    }

    @Test fun applyCatalogKeepsEveryNodeAndArbitrarySelection() {
        val nodeIds = ids(1000)
        val catalog = NodeSelection.parseCatalog(catalogEvent(nodeIds, "gw-0999"))
        val applied = NodeSelection.applyCatalog(ViewState(), catalog, null)

        assertEquals(1000, applied.nodes.size)
        assertEquals(nodeIds, applied.nodes.map { it.id })
        assertEquals("gw-0999", applied.selectedNodeId)
        assertEquals("gw-0999", NodeSelection.connectableNodeId(applied.nodes, applied.selectedNodeId))
        assertEquals(1000, NodeSelection.spinnerPosition(applied.nodes, applied.selectedNodeId))
    }

    @Test fun refreshReorderKeepsSelectionByIdForArbitraryCounts() {
        val forward = ids(1000)
        val first = NodeSelection.applyCatalog(ViewState(),
            NodeSelection.parseCatalog(catalogEvent(forward, "gw-0999")), null)
        val reordered = listOf(forward.last()) + forward.dropLast(1)
        val refreshed = NodeSelection.applyCatalog(first,
            NodeSelection.parseCatalog(catalogEvent(reordered, "gw-0999")), null)

        assertEquals(reordered, refreshed.nodes.map { it.id })
        assertEquals("gw-0999", refreshed.selectedNodeId)
        assertEquals(1, NodeSelection.spinnerPosition(refreshed.nodes, refreshed.selectedNodeId))
    }

    @Test fun selectionMovesBetweenArbitraryIdsAcrossRefreshes() {
        val forward = ids(1000)
        val first = NodeSelection.applyCatalog(ViewState(),
            NodeSelection.parseCatalog(catalogEvent(forward, "gw-0000")), null)
        val moved = NodeSelection.applyCatalog(first,
            NodeSelection.parseCatalog(catalogEvent(forward, "gw-0500")), null)
        val movedAgain = NodeSelection.applyCatalog(moved,
            NodeSelection.parseCatalog(catalogEvent(forward, "gw-0999")), null)

        assertEquals("gw-0500", moved.selectedNodeId)
        assertEquals("gw-0999", movedAgain.selectedNodeId)
        assertEquals(1000, movedAgain.nodes.size)
        assertEquals("gw-0999", NodeSelection.connectableNodeId(movedAgain.nodes, movedAgain.selectedNodeId))
    }

    @Test fun removedSelectedGatewayIsDroppedWithoutFallback() {
        val forward = ids(1000)
        val before = NodeSelection.applyCatalog(ViewState(),
            NodeSelection.parseCatalog(catalogEvent(forward, "gw-0500")), null)
        val remaining = forward.filter { it != "gw-0500" }
        val after = NodeSelection.applyCatalog(before,
            NodeSelection.parseCatalog(catalogEvent(remaining, "gw-0500")), null)

        assertEquals(999, after.nodes.size)
        assertTrue(after.nodes.none { it.id == "gw-0500" })
        assertEquals("gw-0500", after.selectedNodeId)
        assertNull(NodeSelection.displayedNodeId(after.nodes, after.selectedNodeId))
        assertNull(NodeSelection.connectableNodeId(after.nodes, after.selectedNodeId))

        val retained = SessionRetention.onStop(after, "Idle", null)
        assertEquals(999, retained.nodes.size)
        assertNull(RetainedCatalogPolicy.connectableId(retained.phase, retained.nodes, retained.selectedNodeId))
    }

    @Test fun pendingReplacementForRemovedTargetIsCleared() {
        val forward = ids(1000)
        val state = ViewState(selectedNodeId = "gw-0000", pendingNodeId = "gw-0500")
        val kept = NodeSelection.applyCatalog(state,
            NodeSelection.parseCatalog(catalogEvent(forward, "gw-0000")), null)
        assertEquals("gw-0500", kept.pendingNodeId)

        val remaining = forward.filter { it != "gw-0500" }
        val dropped = NodeSelection.applyCatalog(state,
            NodeSelection.parseCatalog(catalogEvent(remaining, "gw-0000")), null)
        assertNull(dropped.pendingNodeId)
        assertEquals("gw-0000", dropped.selectedNodeId)
    }
}

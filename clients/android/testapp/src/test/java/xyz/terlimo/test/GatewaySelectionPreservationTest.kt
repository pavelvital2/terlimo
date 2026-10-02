package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class GatewaySelectionPreservationTest {
    private val a = NodeLabel("gateway-a", "Same name", "XX")
    private val b = NodeLabel("gateway-b", "Same name", "XX")

    @Test fun browseChoiceSurvivesVerifiedReorderWithNoNativeSavedSelection() {
        val browse = ViewState(displayMode = CatalogDisplayMode.BROWSE,
            browseLoaded = true, browseNodes = listOf(a, b), browseSelectedId = a.id)
        val verified = NodeSelection.applyCatalog(browse, NodeCatalog(listOf(b, a), "", "2"), null)
        assertEquals(a.id, verified.selectedNodeId)
        assertEquals(a.id, NodeSelection.connectableNodeId(verified.nodes, verified.selectedNodeId))
        assertEquals(2, NodeSelection.spinnerPosition(verified.nodes, verified.selectedNodeId))
        assertEquals("", verified.browseSelectedId)
    }

    @Test fun removedBrowseChoiceDoesNotBecomeVerifiedConnectableEvenWithSameName() {
        val browse = ViewState(displayMode = CatalogDisplayMode.BROWSE,
            browseLoaded = true, browseNodes = listOf(a, b), browseSelectedId = a.id)
        val verified = NodeSelection.applyCatalog(browse, NodeCatalog(listOf(b), "", "2"), null)
        assertEquals("", verified.selectedNodeId)
        assertNull(NodeSelection.connectableNodeId(verified.nodes, verified.selectedNodeId))
    }

    @Test fun nativeExplicitSelectionRemainsAuthoritative() {
        val browse = ViewState(displayMode = CatalogDisplayMode.BROWSE,
            browseLoaded = true, browseNodes = listOf(a, b), browseSelectedId = a.id)
        val verified = NodeSelection.applyCatalog(browse, NodeCatalog(listOf(a, b), b.id, "2"), null)
        assertEquals(b.id, verified.selectedNodeId)
    }

    @Test fun connectedRefreshKeepsCurrentStableIdAcrossBothProjections() {
        val connected = ViewState(phase = "Connected", nodes = listOf(a, b), selectedNodeId = a.id)
        val browse = BrowseCatalogCodec.apply(connected, BrowseCatalog(listOf(b, a)))
        assertEquals("Connected", browse.phase)
        assertEquals(a.id, BrowseCatalogCodec.selectedId(browse))
        assertTrue(BrowseCatalogCodec.verifiedNodes(browse).isEmpty())
        val verified = NodeSelection.applyCatalog(browse, NodeCatalog(listOf(b, a), a.id, "2"), null)
        assertEquals("Connected", verified.phase)
        assertEquals(a.id, verified.selectedNodeId)
        assertEquals(a.id, NodeSelection.connectableNodeId(verified.nodes, verified.selectedNodeId))
    }

    @Test fun browseRefreshDropsRemovedIdRatherThanMatchingNames() {
        val chosen = ViewState(displayMode = CatalogDisplayMode.BROWSE,
            browseLoaded = true, browseNodes = listOf(a, b), browseSelectedId = a.id)
        val reordered = BrowseCatalogCodec.apply(chosen, BrowseCatalog(listOf(b, a)))
        assertEquals(a.id, BrowseCatalogCodec.selectedId(reordered))
        val removed = BrowseCatalogCodec.apply(reordered, BrowseCatalog(listOf(b)))
        assertEquals("", removed.browseSelectedId)
        assertEquals("", BrowseCatalogCodec.selectedId(removed))
    }

    @Test fun removedConnectedGatewayNeverFallsBackToAnOlderBrowseChoice() {
        val connected = ViewState(phase = "Connected", nodes = listOf(a, b),
            selectedNodeId = a.id, browseSelectedId = b.id)
        val refreshed = BrowseCatalogCodec.apply(connected, BrowseCatalog(listOf(b)))
        assertEquals("", BrowseCatalogCodec.selectedId(refreshed))
        assertEquals(a.id, refreshed.selectedNodeId) // metadata cannot switch a running VPN
        assertTrue(BrowseCatalogCodec.verifiedNodes(refreshed).isEmpty())
    }
}

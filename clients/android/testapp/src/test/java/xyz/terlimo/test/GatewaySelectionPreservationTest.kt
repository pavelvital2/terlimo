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

    @Test fun secondCredentialSnapshotRetainsCurrentDisplayChoice() {
        val browse = ViewState(displayMode = CatalogDisplayMode.BROWSE,
            browseLoaded = true, browseNodes = listOf(a, b), browseSelectedId = a.id)
        val first = NodeSelection.applyCatalog(browse, NodeCatalog(listOf(a, b), "", "1"), null)
        val second = NodeSelection.applyCatalog(first, NodeCatalog(listOf(b, a), "", "2"), null)
        assertEquals(a.id, second.selectedNodeId)
        assertEquals(a.id, NodeSelection.connectableNodeId(second.nodes, second.selectedNodeId))
    }

    @Test fun secondCredentialRemovalClearsChoiceWithoutMatchingSameName() {
        val browse = ViewState(displayMode = CatalogDisplayMode.BROWSE,
            browseLoaded = true, browseNodes = listOf(a, b), browseSelectedId = a.id)
        val first = NodeSelection.applyCatalog(browse, NodeCatalog(listOf(a, b), "", "1"), null)
        val removed = NodeSelection.applyCatalog(first, NodeCatalog(listOf(b), "", "2"), null)
        assertEquals("", removed.selectedNodeId)
        assertNull(NodeSelection.connectableNodeId(removed.nodes, removed.selectedNodeId))
    }

    @Test fun freshBrowseRemovalCannotResurrectStaleCredentialChoice() {
        val credential = ViewState(nodes = listOf(a, b), selectedNodeId = a.id)
        val removed = BrowseCatalogCodec.apply(credential, BrowseCatalog(listOf(b)))
        assertEquals("", BrowseCatalogCodec.selectedId(removed))
        val verified = NodeSelection.applyCatalog(removed, NodeCatalog(listOf(a, b), "", "2"), null)
        assertEquals("", verified.selectedNodeId)
        val browseAgain = BrowseCatalogCodec.apply(removed, BrowseCatalog(listOf(a, b)))
        assertEquals("", BrowseCatalogCodec.selectedId(browseAgain))
    }
}

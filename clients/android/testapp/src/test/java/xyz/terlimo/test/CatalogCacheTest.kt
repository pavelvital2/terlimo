package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertSame
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

class CatalogCacheTest {
    private val nodes = listOf(NodeLabel("a", "Белые списки TEST", "DE"), NodeLabel("b", "TEST2", ""))

    @Test fun codecRoundTripsVerifiedCatalogAndSelection() {
        val encoded = CatalogCacheCodec.encode(RetainedCatalog(nodes, "a", "12"))
        val decoded = CatalogCacheCodec.decode(encoded)
        assertEquals(nodes, decoded?.nodes)
        assertEquals("a", decoded?.selectedNodeId)
        assertEquals("12", decoded?.revision)
    }

    @Test fun codecAcceptsWireValidCountryCodesUpToEight() {
        val codes = listOf(NodeLabel("a", "A", ""), NodeLabel("b", "B", "RU"),
            NodeLabel("c", "C", "RUS"), NodeLabel("d", "D", "ABCDEFGH"))
        val decoded = CatalogCacheCodec.decode(CatalogCacheCodec.encode(RetainedCatalog(codes, "c", "12")))
        assertEquals(codes, decoded?.nodes)
        assertEquals("c", decoded?.selectedNodeId)
        assertNull(CatalogCacheCodec.decode(
            "version\t1\nrevision\t1\nselected\ta\nnode\ta\tA\tABCDEFGHI\n"))
    }

    @Test fun codecRejectsMalformedUnknownOversizedAndDuplicateInput() {
        assertNull(CatalogCacheCodec.decode(""))
        assertNull(CatalogCacheCodec.decode("version\t1\nrevision\t1\nselected\ta\n")) // no nodes
        assertNull(CatalogCacheCodec.decode("version\t2\nrevision\t1\nselected\ta\nnode\ta\tA\tDE\n"))
        assertNull(CatalogCacheCodec.decode("version\t1\nrevision\t1\nselected\ta\nnode\ta\tA\tDE\nextra\tx\n"))
        assertNull(CatalogCacheCodec.decode("version\t1\nrevision\t1\nselected\ta\nnode\ta\tA\tDE\nnode\ta\tA\tDE\n"))
        assertNull(CatalogCacheCodec.decode("version\t1\nrevision\t1\nselected\tz\nnode\ta\tA\tDE\n"))
        val huge = "version\t1\nrevision\t1\nselected\ta\nnode\ta\t" + "x".repeat(70_000) + "\tDE\n"
        assertNull(CatalogCacheCodec.decode(huge))
    }

    @Test fun onStopKeepsCatalogAndSelectionAndClearsPendingState() {
        val prior = ViewState(phase = "CatalogReady", nodes = nodes, selectedNodeId = "b",
            catalogRevision = "12", pendingNodeId = "b", pendingSwitchId = "s", pendingSwitchRevision = "9")
        val stopped = SessionRetention.onStop(prior, "Idle", null)
        assertEquals("Idle", stopped.phase)
        assertEquals(nodes, stopped.nodes)
        assertEquals("b", stopped.selectedNodeId)
        assertEquals("12", stopped.catalogRevision)
        assertNull(stopped.pendingNodeId)
        assertNull(stopped.pendingSwitchId)
        assertNull(stopped.pendingSwitchRevision)
    }

    @Test fun retainedConnectIsAllowedOnlyFromDisconnectedStateWithPresentSelection() {
        assertEquals("b", RetainedCatalogPolicy.connectableId("Idle", nodes, "b"))
        assertEquals("a", RetainedCatalogPolicy.connectableId("Error", nodes, "a"))
        // A removed/absent selection must not fail open.
        assertNull(RetainedCatalogPolicy.connectableId("Idle", nodes, "gone"))
        assertNull(RetainedCatalogPolicy.connectableId("Idle", emptyList(), "a"))
        // The active/connecting phases are not the retained-cache path.
        assertNull(RetainedCatalogPolicy.connectableId("CatalogReady", nodes, "a"))
        assertNull(RetainedCatalogPolicy.connectableId("Connected", nodes, "a"))
    }

    @Test fun retainedCatalogIsShownReadOnlyOnlyWhileDisconnected() {
        assertTrue(CatalogRenderPolicy.showRetained("Idle", nodes))
        assertTrue(CatalogRenderPolicy.showRetained("Error", nodes))
        assertFalse(CatalogRenderPolicy.showRetained("Idle", emptyList()))
        assertFalse(CatalogRenderPolicy.showRetained("CatalogReady", nodes))
        assertFalse(CatalogRenderPolicy.showRetained("Connected", nodes))
        assertTrue(CatalogRenderPolicy.readOnly("Idle"))
        assertTrue(CatalogRenderPolicy.readOnly("Error"))
        assertFalse(CatalogRenderPolicy.readOnly("CatalogReady"))
        assertFalse(CatalogRenderPolicy.readOnly("Connected"))
    }

    @Test fun codecRejectsDelimiterInjectionInNamesIdsOrSelection() {
        val injected = listOf(NodeLabel("a\tb", "A", "DE"))
        var threw = false
        try {
            CatalogCacheCodec.encode(RetainedCatalog(injected, "a", "1"))
        } catch (_: IllegalArgumentException) {
            threw = true
        }
        assertTrue(threw)
        // A crafted extra field must not silently decode into a different structure.
        assertNull(CatalogCacheCodec.decode("version\t1\nrevision\t1\nselected\ta\nnode\ta\tA\tB\tDE\n"))
    }

    @Test fun freshProcessActivityProjectionHydratesRetainedCatalogForConnectAction() {
        val empty = ViewState()
        val raw = CatalogCacheCodec.encode(RetainedCatalog(nodes, "b", "12"))
        val projected = RetainedProjection.hydrate(empty, raw)
        assertEquals(nodes, projected.nodes)
        assertEquals("b", projected.selectedNodeId)
        assertEquals("12", projected.catalogRevision)
        // The Connect action path is available from the hydrated projection only.
        assertEquals("b", RetainedCatalogPolicy.connectableId(projected.phase, projected.nodes, projected.selectedNodeId))
        assertNull(RetainedCatalogPolicy.connectableId(empty.phase, empty.nodes, empty.selectedNodeId))
        // Live state always wins over the durable cache.
        val live = ViewState(phase = "CatalogReady", nodes = listOf(NodeLabel("x", "X", "")), selectedNodeId = "x")
        assertEquals("x", RetainedProjection.hydrate(live, raw).selectedNodeId)
    }

    @Test fun cacheIsDisplayOnlyAndCarriesNoAccessGrant() {
        val encoded = CatalogCacheCodec.encode(RetainedCatalog(nodes, "a", "12"))
        assertFalse(encoded.contains("grant"))
        assertFalse(encoded.contains("expires"))
        assertTrue(RetainedCatalogPolicy.connectableId("Idle", nodes, "a") != null)
    }

    private fun labels(count: Int, name: (Int) -> String = { "Server " + it.toString().padStart(4, '0') }): List<NodeLabel> =
        (0 until count).map {
            NodeLabel("gw-" + it.toString().padStart(4, '0'), name(it), if (it % 2 == 0) "DE" else "")
        }

    private fun oversizedRaw(): String = buildString {
        append("version\t1\nrevision\t1\nselected\tgw-0000\n")
        repeat(3000) { append("node\tgw-${it.toString().padStart(4, '0')}\tServer ${it.toString().padStart(4, '0')}\tDE\n") }
    }

    @Test fun codecRoundTripsThreeAndThousandNodeCatalogs() {
        for (count in listOf(3, 1000)) {
            val nodes = labels(count)
            val selected = nodes[count / 2].id
            val encoded = CatalogCacheCodec.encode(RetainedCatalog(nodes, selected, "rev-$count"))
            val decoded = CatalogCacheCodec.decode(encoded)

            assertEquals(count, decoded?.nodes?.size)
            assertEquals(nodes, decoded?.nodes)
            assertEquals(selected, decoded?.selectedNodeId)
            assertEquals("rev-$count", decoded?.revision)
            assertEquals(selected, RetainedCatalogPolicy.connectableId("Idle", decoded!!.nodes, decoded.selectedNodeId))
        }
    }

    @Test fun refreshReorderRoundtripKeepsSelectionById() {
        val nodes = labels(1000)
        val reordered = nodes.reversed()
        val decoded = CatalogCacheCodec.decode(
            CatalogCacheCodec.encode(RetainedCatalog(reordered, nodes.first().id, "r2")))

        assertEquals(reordered, decoded?.nodes)
        assertEquals(nodes.size, NodeSelection.spinnerPosition(decoded!!.nodes, decoded.selectedNodeId))
        assertEquals(nodes.first(), decoded.nodes.last())
    }

    @Test fun removalRoundtripDropsRemovedGateway() {
        val nodes = labels(1000)
        val removed = nodes.filter { it.id != "gw-0500" }

        val cleared = CatalogCacheCodec.decode(CatalogCacheCodec.encode(RetainedCatalog(removed, "", "r3")))
        assertEquals(999, cleared?.nodes?.size)
        assertEquals("", cleared?.selectedNodeId)
        assertTrue(cleared!!.nodes.none { it.id == "gw-0500" })

        // A selection still naming the removed gateway is never restored.
        assertNull(CatalogCacheCodec.decode(CatalogCacheCodec.encode(RetainedCatalog(removed, "gw-0500", "r3"))))
    }

    @Test fun malformedAndTooLargeDecodeReturnsNullWithoutThrowing() {
        val cases = listOf(
            oversizedRaw(),
            "version\t1\nrevision\t1\nselected\t\t",
            "version\t1\nrevision\t1\nselected\t\nnode\tgw-0\tS\tDE",
            "version\t1\nrevision\t1\nselected\tgw-0\nnode\tgw-0\tA\tB\tDE\n",
            "version\t1\nrevision\t1\nselected\tgw-0\nnode\tgw-0\t" + "N".repeat(101) + "\tDE\n",
            "version\t1\nrevision\t1\nselected\tgw-0\nnode\tgw-0\tS\tde\n",
            "version\t1\nrevision\t1\nselected\tgw-0\nnode\tgw-0\tS\tDE\nnode\tgw-0\tS\tDE\n",
        )
        val outcomes = cases.map { runCatching { CatalogCacheCodec.decode(it) } }
        assertTrue(outcomes.all { it.isSuccess && it.getOrNull() == null })
    }

    @Test fun oversizedEncodeThrowsAndPriorFileSurvives() {
        val prior = CatalogCacheCodec.encode(RetainedCatalog(labels(3), "gw-0000", "prior"))
        val tooBig = RetainedCatalog(labels(1000) { "N".repeat(100) }, "gw-0000", "big")

        assertThrows(IllegalArgumentException::class.java) { CatalogCacheCodec.encode(tooBig) }
        assertEquals(labels(3), CatalogCacheCodec.decode(prior)?.nodes)
    }

    @Test fun serviceSeedCompositionRestoresLastGoodForThreeAndThousand() {
        for (count in listOf(3, 1000)) {
            val nodes = labels(count)
            val selected = nodes[count / 2].id
            val raw = CatalogCacheCodec.encode(RetainedCatalog(nodes, selected, "rev-$count"))
            val view = ViewState()

            val seeded = CatalogCacheCodec.decode(raw)?.takeIf { it.nodes.isNotEmpty() }?.let {
                view.copy(nodes = it.nodes, selectedNodeId = it.selectedNodeId, catalogRevision = it.revision)
            } ?: view

            assertEquals(nodes, seeded.nodes)
            assertEquals(selected, seeded.selectedNodeId)
            assertEquals("rev-$count", seeded.catalogRevision)
            assertEquals(selected, RetainedCatalogPolicy.connectableId(seeded.phase, seeded.nodes, seeded.selectedNodeId))
            assertEquals(nodes, RetainedProjection.hydrate(view, raw).nodes)
        }
    }

    @Test fun corruptedOrOversizedCacheLeavesPreviousViewUntouched() {
        val live = ViewState(phase = "CatalogReady", nodes = labels(1000),
            selectedNodeId = "gw-0500", catalogRevision = "last-good")

        for (raw in listOf("garbage", "version\t9\nrevision\t1\nselected\tgw-0000\nnode\tgw-0000\tS\tDE\n", oversizedRaw())) {
            assertNull(CatalogCacheCodec.decode(raw))
            val seeded = CatalogCacheCodec.decode(raw)?.let {
                live.copy(nodes = it.nodes, selectedNodeId = it.selectedNodeId, catalogRevision = it.revision)
            } ?: live
            assertSame(live, seeded)
            assertSame(live, RetainedProjection.hydrate(live, raw))
            assertEquals(1000, live.nodes.size)
            assertEquals("gw-0500", live.selectedNodeId)
            assertEquals("last-good", live.catalogRevision)
        }
    }
}

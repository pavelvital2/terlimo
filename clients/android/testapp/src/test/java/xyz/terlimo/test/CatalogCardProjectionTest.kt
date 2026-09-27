package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class CatalogCardProjectionTest {
    private val nodes = listOf(
        CatalogCardInput("a", "Москва", "Россия"),
        CatalogCardInput("b", "Франкфурт", "Германия"),
    )

    @Test fun projectionPreservesOrderAndNeverInfersSelection() {
        val result = CatalogCardProjection.project(nodes, null)
        assertEquals(listOf("a", "b"), result.cards.map { it.nodeId })
        assertNull(result.selectedNodeId)
        assertEquals(SelectionProblem.REQUIRED, result.selectionProblem)
        assertTrue(result.cards.none { it.selected })
    }

    @Test fun explicitSelectionSurvivesReorder() {
        val result = CatalogCardProjection.project(nodes.reversed(), "b")
        assertEquals("b", result.selectedNodeId)
        assertEquals(listOf(true, false), result.cards.map { it.selected })
    }

    @Test fun removedSelectionStopsAndAsksWithoutFallback() {
        val result = CatalogCardProjection.project(listOf(nodes.first()), "b")
        assertNull(result.selectedNodeId)
        assertEquals(SelectionProblem.SAVED_NODE_REMOVED, result.selectionProblem)
        assertTrue(result.cards.none { it.selected })
    }

    @Test fun semanticsContainTextualStateAndTouchTarget() {
        // A direct-fallback answer carries both values: only the echo RTT is displayed.
        val pings = mapOf("a" to NodePingState.Success(42, 900), "b" to NodePingState.Timeout)
        val result = CatalogCardProjection.project(nodes, "a", pings)
        assertTrue(result.cards[0].contentDescription.contains("Россия, Москва, Выбран, Доступен, 42 мс"))
        assertEquals(42L, result.cards[0].rttMs)
        assertFalse(result.cards[0].stateText.contains("900"))
        assertFalse(result.cards[0].contentDescription.contains("900"))
        assertTrue(result.cards[1].stateText.contains("Тайм-аут"))
        assertTrue(result.cards.all { it.minimumTouchTargetDp >= 48 })
    }

    @Test fun pingResultNeverBlocksExplicitSelection() {
        val result = CatalogCardProjection.project(nodes, null, mapOf("a" to NodePingState.Timeout, "b" to NodePingState.Failed))
        assertEquals("a", CatalogCardProjection.explicitSelection(result, "a"))
        assertEquals("b", CatalogCardProjection.explicitSelection(result, "b"))
        assertNull(CatalogCardProjection.explicitSelection(result, "missing"))
    }

    @Test fun blankDuplicateAndOversizedFieldsReject() {
        assertThrows(IllegalArgumentException::class.java) { CatalogCardProjection.project(listOf(CatalogCardInput("", "A", "RU")), null) }
        assertThrows(IllegalArgumentException::class.java) { CatalogCardProjection.project(listOf(nodes[0], nodes[0]), null) }
        assertThrows(IllegalArgumentException::class.java) { CatalogCardProjection.project(listOf(CatalogCardInput("a", "x".repeat(101), "RU")), null) }
    }
}

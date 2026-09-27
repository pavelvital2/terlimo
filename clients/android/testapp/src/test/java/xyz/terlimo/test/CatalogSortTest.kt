package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Test

class CatalogSortTest {
    private fun card(id: String, country: String, availability: NodeAvailability, rttMs: Long? = null) =
        CatalogCard(
            nodeId = id, name = id, country = country, availability = availability,
            rttMs = rttMs, selected = false, enabled = true, stateText = "", contentDescription = "",
        )

    @Test
    fun `measured available nodes come first ascending by probe time`() {
        val ranked = CatalogSort.ranked(listOf(
            card("slow", "DE", NodeAvailability.AVAILABLE, 300),
            card("down", "DE", NodeAvailability.UNAVAILABLE),
            card("fast", "RU", NodeAvailability.AVAILABLE, 40),
            card("timeout", "RU", NodeAvailability.TIMEOUT),
            card("unknown", "FR", NodeAvailability.UNKNOWN),
        )).map { it.nodeId }
        assertEquals(listOf("fast", "slow", "timeout", "down", "unknown"), ranked)
    }

    @Test
    fun `missing measured rtt never leads a measured node`() {
        val ranked = CatalogSort.ranked(listOf(
            card("unmeasured", "RU", NodeAvailability.AVAILABLE, null),
            card("available", "RU", NodeAvailability.AVAILABLE, null),
            card("measured", "DE", NodeAvailability.AVAILABLE, 500),
            card("timeout", "FR", NodeAvailability.TIMEOUT),
        )).map { it.nodeId }
        assertEquals(listOf("measured", "available", "unmeasured", "timeout"), ranked)
    }

    @Test
    fun `stable ties break by country then name then node id`() {
        val ranked = CatalogSort.ranked(listOf(
            card("z", "RU", NodeAvailability.AVAILABLE, 50),
            card("a", "DE", NodeAvailability.AVAILABLE, 50),
            card("b", "DE", NodeAvailability.AVAILABLE, 50),
        )).map { it.nodeId }
        assertEquals(listOf("a", "b", "z"), ranked)
    }
}

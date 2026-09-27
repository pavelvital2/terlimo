package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class LinkPropertyDiffTest {
    @Test fun reportsOnlyFieldNamesNeverValues() {
        val before = mapOf("DNS" to "synthetic-private-dns", "ROUTES" to "synthetic-old-route")
        val after = mapOf("DNS" to "synthetic-new-dns", "ROUTES" to "synthetic-new-route")
        assertEquals(setOf("DNS", "ROUTES"), LinkPropertyDiff.changed(before, after, false))
    }
    @Test fun unknownPlatformDifferenceIsNotCalledRestored() {
        assertEquals(setOf("UNOBSERVED_FIELDS"), LinkPropertyDiff.changed(emptyMap(), emptyMap(), false))
        assertTrue(LinkPropertyDiff.changed(emptyMap(), emptyMap(), true).isEmpty())
        assertEquals(setOf("NETWORK_UNAVAILABLE"), LinkPropertyDiff.changed(null, emptyMap(), false))
    }
}

package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/** §26.5 exactly-once terminal delivery used by every rejection path. */
class CatalogRefreshRequestRegistryTest {

    private class Probe : CatalogRefreshObserver {
        val events = mutableListOf<Triple<String, Boolean, String?>>()
        override fun onCompleted(requestId: String, ok: Boolean, error: String?) {
            events += Triple(requestId, ok, error)
        }
    }

    @Test fun registerIsSingleAndDeliveryTakesOnce() {
        val registry = CatalogRefreshRequestRegistry()
        val first = Probe()
        val second = Probe()
        assertTrue(registry.register("r1", first))
        assertFalse(registry.register("r1", second))
        registry.deliver("r1", ok = false, error = "rejected")
        registry.deliver("r1", ok = true, error = null)
        assertEquals(listOf(Triple("r1", false, "rejected")), first.events)
        assertTrue(second.events.isEmpty())
        assertNull(registry.take("r1"))
    }

    @Test fun unknownDeliveryIsANoOp() {
        val registry = CatalogRefreshRequestRegistry()
        registry.deliver("missing", ok = true, error = null)
    }
}

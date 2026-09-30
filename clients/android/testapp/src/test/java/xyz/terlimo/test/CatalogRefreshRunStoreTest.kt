package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/** §26.5 per-run resource bookkeeping of the JobService wrapper. */
class CatalogRefreshRunStoreTest {

    @Test fun lateBinderOfAnOldRunIsRefused() {
        val store = CatalogRefreshRunStore()
        val tag = Any()
        store.start(1L, tag)
        store.bind(1L, "connection")
        assertTrue(store.setBinder(1L, "binder"))
        store.end(1L)
        assertFalse(store.setBinder(1L, "late"))
        assertNull(store.run(1L))
        assertNull(store.tokenFor(tag))
    }

    @Test fun finishAndEndAreIdempotent() {
        val store = CatalogRefreshRunStore()
        store.start(1L, Any())
        assertNotNull(store.markFinished(1L))
        assertNull(store.markFinished(1L))
        assertNotNull(store.end(1L))
        assertNull(store.end(1L))
    }

    @Test fun failedBindLeavesNoConnectionAndNoBoundState() {
        val store = CatalogRefreshRunStore()
        store.start(1L, Any())
        store.bindFailed(1L)
        val run = store.run(1L)!!
        assertNull(run.connection)
        assertFalse(run.bound)
    }

    @Test fun stopKeepsTheBinderAvailableUntilUnbind() {
        val store = CatalogRefreshRunStore()
        store.start(1L, Any())
        store.bind(1L, "connection")
        store.setBinder(1L, "binder")
        // Cancellation must be able to use the binder before cleanup clears it.
        assertEquals("binder", store.run(1L)!!.binder)
        store.clearBinding(1L)
        assertNull(store.run(1L)!!.binder)
    }
    @Test fun detachedResourcesRemainAvailableForCleanupButLateCallbacksAreRejected() {
        val store = CatalogRefreshRunStore()
        store.start(1L, Any())
        store.bind(1L, "connection")
        store.setBinder(1L, "binder")
        val detached = store.end(1L)!!
        assertTrue(detached.bound)
        assertEquals("connection", detached.connection)
        assertEquals("binder", detached.binder)
        assertFalse(store.setBinder(1L, "late"))
        store.start(2L, Any())
        store.bind(2L, "second")
        store.clearBinding(2L)
        assertFalse(store.setBinder(2L, "late"))
    }

}

package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** Pins the wiring that keeps the verified catalog across an ordinary Disconnect. */
class CatalogRetentionSourceTest {
    private val root = File("src/main/java/xyz/terlimo/test")
    private val service = File(root, "SessionService.kt").readText()
    private val activity = File(root, "MainActivity.kt").readText()

    @Test fun disconnectPublishesRetainedCatalogNotAFreshEmptyState() {
        val stop = service.substringAfter("private fun stopAttempt(code: String?)")
            .substringBefore("private fun persistCatalogCache")
            .substringBefore("},\n", missingDelimiterValue = "")
        assertTrue(service.contains("SessionRetention.onStop(view,"))
        assertFalse(stop.contains("publish(ViewState("))
    }

    @Test fun verifiedCatalogIsPersistedForRecreation() {
        assertTrue(service.contains("persistCatalogCache(updated)"))
        assertTrue(service.contains("storage.readCatalogCache()"))
        assertTrue(service.contains("storage.writeCatalogCache("))
    }

    @Test fun retainedCatalogIsRenderedReadOnlyWhileDisconnected() {
        val catalog = File(root, "ServerCatalogView.kt").readText()
        assertTrue(catalog.contains("CatalogRenderPolicy.showRetained(state.phase, state.nodes)"))
        assertTrue(catalog.contains("addContent(state, readOnly = true)"))
        assertTrue(catalog.contains("addServerRow(it, readOnly)"))
    }

    @Test fun retainedConnectStartsFreshAttemptAndStaysAttemptFenced() {
        assertTrue(activity.contains("RetainedCatalogPolicy.connectableId("))
        assertTrue(activity.contains("setAction(\"connect\")"))
        val auto = service.substringAfter("private fun autoConnectRetained(")
            .substringBefore("private fun stopAttempt")
        assertTrue(auto.contains("if (gate.active != attempt || stopping.get()) return"))
        assertTrue(auto.contains("connectOnCatalog = null"))
        assertTrue(auto.contains("send(JSONObject().put(\"type\", \"select_node\")"))
    }

    @Test fun freshProcessActivityHydratesWithoutStartingServiceForDisplay() {
        assertTrue(activity.contains("RetainedProjection.hydrate(SessionService.view,"))
        assertTrue(activity.contains("private fun projectedState(): ViewState ="))
        assertTrue(activity.contains("startForegroundService(Intent(this, SessionService::class.java).setAction(\"connect\")"))
    }
}

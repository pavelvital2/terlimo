package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test

class CatalogErrorActionsTest {
    private fun waitingBrowse() = ViewState(
        phase = "BootstrapConnecting", displayMode = CatalogDisplayMode.BROWSE)

    @Test fun browseOfflineMirrorsAllThreeCodesAndOffersTheCardActions() {
        listOf("TRANSPORT", "SERVICE_UNAVAILABLE", "RATE_LIMITED").forEach { code ->
            val mirrored = BrowseErrorPolicy.fromStderr(code, waitingBrowse())
            assertEquals(code, mirrored)
            assertTrue(code, CatalogErrorActions.browseRetry(mirrored))
        }
    }

    @Test fun browseWithoutErrorHasNoActions() {
        assertFalse(CatalogErrorActions.browseRetry(null))
    }

    @Test fun credentialCatalogueErrorOffersActions() {
        listOf("CATALOG_TIMEOUT", "TRANSPORT", "SERVICE_UNAVAILABLE", "RATE_LIMITED").forEach {
            assertTrue(it, CatalogErrorActions.credentialRetry("Error", it))
        }
    }

    @Test fun successIdleAndConnectedSwitchFailureHaveNoUpdateLabel() {
        assertFalse(CatalogErrorActions.credentialRetry("CatalogReady", null))
        assertFalse(CatalogErrorActions.credentialRetry("Error", null))
        // A connected server-switch failure is not an update error.
        assertFalse(CatalogErrorActions.credentialRetry("Connected", "TRANSPORT"))
        assertFalse(CatalogErrorActions.credentialRetry("Connected", "NODE_UNAVAILABLE"))
    }

    @Test fun nonCatalogueErrorsAreNotRetryableAsUpdates() {
        listOf("SUBSCRIPTION_EXPIRED", "GRANT_REVOKED", "IMPORT_REQUIRED").forEach {
            assertFalse(it, CatalogErrorActions.credentialRetry("Error", it))
        }
    }

    @Test fun offlineMirrorRequiresTheWaitingBrowseState() {
        assertNotNull(BrowseErrorPolicy.fromStderr("TRANSPORT", waitingBrowse()))
        val connected = ViewState(phase = "Connected", displayMode = CatalogDisplayMode.BROWSE)
        assertEquals(null, BrowseErrorPolicy.fromStderr("TRANSPORT", connected))
    }
}

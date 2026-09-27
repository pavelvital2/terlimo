package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class NodeAuthenticatingUiProjectionTest {
    @Test fun nodeAuthenticationKeepsConnectingControlsVisible() {
        val root = File("src/main/java/xyz/terlimo/test")
        val orbit = File(root, "OrbitHomeHeader.kt").readText()
        val activity = File(root, "MainActivity.kt").readText()

        assertTrue(orbit.contains("\"BootstrapConnecting\", \"NodeAuthenticating\", \"ConfiguringVPN\""))
        assertTrue(activity.contains("\"BootstrapConnecting\", \"NodeAuthenticating\", \"ConfiguringVPN\""))
    }

    @Test fun nodeAuthenticationAndVpnConfigurationKeepCatalogVisible() {
        assertTrue(isCatalogUsable("CatalogReady"))
        assertTrue(isCatalogUsable("NodeAuthenticating"))
        assertTrue(isCatalogUsable("ConfiguringVPN"))
        assertTrue(isCatalogUsable("Connected"))
        assertTrue(isCatalogUsable("SwitchingServer"))
        assertTrue(isCatalogUsable("SleepPaused"))
    }

    @Test fun catalogIsNotUsableBeforeOpeningOrAfterError() {
        assertFalse(isCatalogUsable("Idle"))
        assertFalse(isCatalogUsable("Error"))
    }
}

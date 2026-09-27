package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class RoutingEditGateTest {
    private fun snapshot(phase: String, tunnel: TunnelApplicationState) = RoutingEditSnapshot(phase, tunnel)

    @Test fun normalCatalogReadyWithoutVpnAllowsSave() {
        assertTrue(RoutingEditGate.allowsSave(snapshot("CatalogReady", TunnelApplicationState.NONE)))
        assertTrue(RoutingEditGate.allowsSave(snapshot("Idle", TunnelApplicationState.NONE)))
        assertTrue(RoutingEditGate.allowsSave(snapshot("Error", TunnelApplicationState.NONE)))
    }

    @Test fun retainedRecoveryOrSleepTunnelForbidsSaveEvenAtCatalogReady() {
        // Recovery/sleep-resume keeps the previous WG tunnel UP while the phase can be CatalogReady.
        assertFalse(RoutingEditGate.allowsSave(snapshot("CatalogReady", TunnelApplicationState.APPLIED)))
        assertFalse(RoutingEditGate.allowsSave(snapshot("CatalogReady", TunnelApplicationState.APPLYING)))
        assertFalse(RoutingEditGate.allowsSave(snapshot("Idle", TunnelApplicationState.APPLIED)))
        assertFalse(RoutingEditGate.allowsSave(snapshot("Error", TunnelApplicationState.APPLYING)))
    }

    @Test fun retainedRecoveryTransitionsToNoneAfterSuccessfulTeardown() {
        // Retained recovery/sleep: WG applied and native not confirmed stopped.
        val held = RoutingEditState.tunnelOf(
            activeVpnConfig = true, sleepPaused = false, networkRecovery = false,
            nativeStopped = false, applying = false)
        assertEquals(TunnelApplicationState.APPLIED, held)
        assertFalse(RoutingEditGate.allowsSave(RoutingEditSnapshot("CatalogReady", held)))

        // Successful teardown: child stopped + WG DOWN + markers cleared -> Idle/Error with NONE.
        val after = RoutingEditState.tunnelOf(
            activeVpnConfig = false, sleepPaused = false, networkRecovery = false,
            nativeStopped = true, applying = false)
        assertEquals(TunnelApplicationState.NONE, after)
        assertTrue(RoutingEditGate.allowsSave(RoutingEditSnapshot("Idle", after)))
        assertTrue(RoutingEditGate.allowsSave(RoutingEditSnapshot("Error", after)))
    }

    @Test fun applyingAndHeldMarkersRemainDenied() {
        assertEquals(
            TunnelApplicationState.APPLYING,
            RoutingEditState.tunnelOf(false, sleepPaused = false, networkRecovery = false, nativeStopped = true, applying = true))
        assertFalse(RoutingEditGate.allowsSave(RoutingEditSnapshot("CatalogReady", TunnelApplicationState.APPLYING)))
        assertFalse(RoutingEditGate.allowsSave(RoutingEditSnapshot("Idle", TunnelApplicationState.APPLIED)))
        assertFalse(RoutingEditGate.allowsSave(RoutingEditSnapshot("CatalogReady", TunnelApplicationState.APPLIED)))
    }

    @Test fun configuringStoppingSleepAndConnectedAreForbidden() {
        listOf(
            "ConfiguringVPN", "NodeAuthenticating", "SwitchingServer", "Stopping", "SleepPaused",
            "Reconnecting", "BootstrapConnecting", "Connected", "KillSwitch", "WaitingUser",
        ).forEach { phase ->
            assertFalse(phase, RoutingEditGate.allowsSave(snapshot(phase, TunnelApplicationState.NONE)))
        }
        assertFalse(RoutingEditGate.allowsSave(snapshot("SleepPaused", TunnelApplicationState.APPLIED)))
    }
}

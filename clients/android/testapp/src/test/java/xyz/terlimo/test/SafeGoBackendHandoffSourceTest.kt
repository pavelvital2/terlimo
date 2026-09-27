package xyz.terlimo.test

import java.nio.file.Files
import java.nio.file.Paths
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Regression guard for the runtime long-loss failure (2026-09-20): the recovery resume reconfigured
 * the SAME SafeGoBackend via setState(UP), whose old handoff called setStateInternal(DOWN) and thus
 * VpnService.stopSelf(). The asynchronous onDestroy then tore down the freshly established TUN, so
 * the resumed native attempt hit its own VPN_SETUP_TIMEOUT with no protection left (`vpn=0 tun=0`).
 *
 * Android VpnService cannot run in a JVM unit test, so this test pins the exact handoff contract;
 * the protection-policy behaviour is covered by TerminalFailurePolicyTest and the recovery
 * transition tests by PhysicalNetworkRecoveryTest.
 */
class SafeGoBackendHandoffSourceTest {
    private val source = String(Files.readAllBytes(
        Paths.get("src/main/java/xyz/terlimo/test/SafeGoBackend.java")), Charsets.UTF_8)

    @Test fun upHandoffRetiresOnlyTheUserspaceHandle() {
        val up = source.substringAfter("if (state == Tunnel.State.UP) {")
            .substringBefore("} else if (state == Tunnel.State.DOWN")
        assertTrue(up.contains("retireState(currentTunnel)"))
        // The in-app handoff must not stop the VpnService that immediately re-establishes the TUN.
        assertFalse(up.contains("setStateInternal(currentTunnel, null, Tunnel.State.DOWN)"))
    }

    @Test fun explicitDownStillStopsTheVpnService() {
        val down = source.substringAfter("} else if (state == Tunnel.State.DOWN && tunnel == currentTunnel) {")
            .substringBefore("return getState(tunnel);")
        assertTrue(down.contains("setStateInternal(tunnel, null, Tunnel.State.DOWN)"))
        assertTrue(source.contains("vpnService.get(0, TimeUnit.NANOSECONDS).stopSelf()"))
    }
}

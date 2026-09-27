package xyz.terlimo.test

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class BatchPingControlTest {
    @Test
    fun `control renders for a usable catalog pre-connect and while Connected`() {
        assertTrue(BatchPingControl.shouldRender(readOnly = false, phase = "CatalogReady", nodeCount = 3))
        assertTrue(BatchPingControl.shouldRender(readOnly = false, phase = "Connected", nodeCount = 1))
    }

    @Test
    fun `read-only retained catalog never renders the control`() {
        assertFalse(BatchPingControl.shouldRender(readOnly = true, phase = "CatalogReady", nodeCount = 3))
        assertFalse(BatchPingControl.shouldRender(readOnly = true, phase = "Connected", nodeCount = 3))
    }

    @Test
    fun `idle error switching and empty catalog never render the control`() {
        listOf("Idle", "Error", "SwitchingServer", "Starting", "SleepPaused", "KillSwitch").forEach { phase ->
            assertFalse(phase, BatchPingControl.shouldRender(false, phase, 3))
        }
        assertFalse(BatchPingControl.shouldRender(false, "Connected", 0))
        assertFalse(BatchPingControl.shouldRender(false, "CatalogReady", 0))
    }
}

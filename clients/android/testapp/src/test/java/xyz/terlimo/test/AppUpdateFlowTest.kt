package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class AppUpdateFlowTest {
    @Test fun eachConnectionGetsCheckButDuplicateProjectionDoesNot() {
        val gate = AppUpdateConnectionGate()
        val flow = AppUpdateFlow()
        gate.begin("a", true)
        assertFalse(gate.accepted("foreign"))
        if (gate.accepted("a")) flow.connection("a")
        assertFalse(gate.accepted("a"))
        assertNotNull(flow.next())
        assertNull(flow.next())
        gate.begin("b", true)
        if (gate.accepted("b")) flow.connection("b")
        flow.connection("b")
        assertNull(flow.next()) // only one request in flight
        flow.finished()
        assertNotNull(flow.next()) // distinct connection not lost to singleflight
        flow.finished()
        assertNull(flow.next())
        gate.begin("background", false)
        assertFalse(gate.accepted("background"))
    }
    @Test fun manualCheckReoffersDismissedVersionWithoutRenderLoop() {
        val flow = AppUpdateFlow()
        flow.targetVersion = 15
        assertTrue(flow.shouldOffer(15, false))
        flow.notifiedVersion = 15
        assertFalse(flow.shouldOffer(15, false))
        flow.later()
        assertFalse(flow.shouldOffer(15, false))
        flow.manual(); flow.manual()
        assertTrue(flow.next()!!.manual)
        assertTrue(flow.shouldOffer(15, true))
        flow.finished(); assertNull(flow.next())
        assertTrue(flow.shouldOffer(16, false))
    }
    @Test fun permissionReturnIsOneShotAndNeverMeansInstalled() {
        val flow = AppUpdateFlow()
        flow.targetVersion = 15; flow.stage = "permission"
        assertFalse(flow.permissionReturned(false)); assertEquals("ready", flow.stage)
        assertFalse(flow.permissionReturned(true))
        flow.stage = "permission"
        assertTrue(flow.permissionReturned(true)); assertEquals("ready", flow.stage)
        assertFalse(flow.permissionReturned(true))
        flow.stage = "installing"
        assertEquals("installing", flow.reconcile(14, true))
        assertEquals("installed", flow.reconcile(15, true))
    }
    @Test fun restartAndCancellationNeverAutoRestartDownloadOrAcceptMissingFile() {
        val flow = AppUpdateFlow()
        flow.targetVersion = 15; flow.stage = "downloading"
        assertEquals("retry", flow.reconcile(14, false))
        for (stage in listOf("ready", "permission", "installing")) {
            flow.stage = stage
            assertEquals("retry", flow.reconcile(14, false))
        }
        flow.stage = "downloading"; flow.cancelled()
        assertEquals("retry", flow.stage)
        assertNull(flow.next())
    }
}

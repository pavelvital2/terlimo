package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ProbeResultTest {
    private fun event(status: String, extra: String = "") =
        JSONObject("""{"v":1,"attempt_id":"a","type":"node_probe_result","node_id":"n1","status":"$status"$extra}""")

    @Test
    fun `rtt_ms is the measured RTT and is required for an ok frame`() {
        val rtt = ProbeResult.parse(event("ok", ""","rtt_ms":42""")) as NodePingState.Success
        assertEquals(42L, rtt.rttMs)
        assertNull(rtt.setupMs)
        // A setup-only frame is not a measured RTT and must not be shown as one.
        assertTrue(runCatching { ProbeResult.parse(event("ok", ""","transport_setup_ms":900""")) }.isFailure)
    }

    @Test
    fun `success frame with both fields keeps rtt as the measurement and setup diagnostic`() {
        val both = ProbeResult.parse(event("ok", ""","transport_setup_ms":3100,"rtt_ms":57""")) as NodePingState.Success
        assertEquals(57L, both.rttMs)
        assertEquals(3100L, both.setupMs)
    }

    @Test
    fun `non-ok status must not carry measurement fields`() {
        assertTrue(runCatching { ProbeResult.parse(event("timeout", ""","rtt_ms":1""")) }.isFailure)
    }

    @Test
    fun `busy is its own honest state, never a measurement`() {
        assertTrue(ProbeResult.parse(event("busy")) is NodePingState.Busy)
        assertTrue(ProbeResult.parse(event("timeout")) is NodePingState.Timeout)
        assertTrue(ProbeResult.parse(event("cancelled")) is NodePingState.Cancelled)
    }

    @Test
    fun `unknown key or status is rejected`() {
        assertTrue(runCatching { ProbeResult.parse(event("ok", ""","other":1,"rtt_ms":5""")) }.isFailure)
        assertTrue(runCatching { ProbeResult.parse(event("weird")) }.isFailure)
    }

    @Test
    fun `connected current-node probe is a start and non-current busy is not fake success`() {
        assertEquals(ProbeGate.Tap.START, ProbeGate.single("Connected", true, false, false, false))
        assertEquals(ProbeGate.Tap.CANCEL, ProbeGate.single("Connected", true, true, false, true))
        assertEquals(ProbeGate.Tap.IGNORE, ProbeGate.single("Connected", true, false, true, false))
    }

    @Test
    fun `all-node ping works both pre-connect and while Connected`() {
        assertEquals(ProbeGate.AllTap.START, ProbeGate.all("CatalogReady", false, false, 3))
        assertEquals(ProbeGate.AllTap.START, ProbeGate.all("Connected", false, false, 3))
        assertEquals(ProbeGate.AllTap.IGNORE, ProbeGate.all("Connected", true, false, 3))
        assertEquals(ProbeGate.AllTap.IGNORE, ProbeGate.all("Idle", false, false, 3))
    }
}

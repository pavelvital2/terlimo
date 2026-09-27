package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Test

class TrafficTextTest {
    private fun snap(rx: Long, tx: Long, rxRate: Long, txRate: Long, measured: Boolean) =
        TrafficSnapshot(active = true, rxTotal = rx, txTotal = tx, rxRateBps = rxRate, txRateBps = txRate, measured = measured)

    @Test
    fun `bytes use explicit binary units`() {
        assertEquals("512 Б", TrafficText.bytes(512))
        assertEquals("1.0 КБ", TrafficText.bytes(1024))
        assertEquals("1.5 МБ", TrafficText.bytes(1024L * 1024 + 524288))
        assertEquals("1.00 ГБ", TrafficText.bytes(1024L * 1024 * 1024))
    }

    @Test
    fun `rate uses per-second units with download and upload order`() {
        assertEquals("900 Б/с", TrafficText.rate(900))
        assertEquals("1.5 КБ/с", TrafficText.rate(1536))
        assertEquals("2.0 МБ/с", TrafficText.rate(2 * 1024 * 1024))
    }

    @Test
    fun `unmeasured sample never fabricates a measured rate`() {
        assertEquals("↓ — · ↑ —", TrafficText.speedLine(snap(5000, 1000, 0, 0, measured = false)))
        // Totals stay as the last known values.
        assertEquals("↓ 4.9 КБ · ↑ 1000 Б", TrafficText.trafficLine(snap(5000, 1000, 0, 0, measured = false)))
    }

    @Test
    fun `measured sample shows totals and rates with RX first`() {
        val t = snap(2048, 1024, 512, 128, measured = true)
        assertEquals("↓ 2.0 КБ · ↑ 1.0 КБ", TrafficText.trafficLine(t))
        assertEquals("↓ 512 Б/с · ↑ 128 Б/с", TrafficText.speedLine(t))
    }
}

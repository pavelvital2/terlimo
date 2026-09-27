package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Connect -> traffic -> disconnect behavior for the §07.4 display/notification data:
 * local live counters stay distinct from the server credited totals, an unmeasured sample
 * is an honest unknown, and a disconnect keeps the last known totals but marks them stale.
 */
class TrafficDisplayLifecycleTest {
    private fun usage(fetchedAt: Long) = (UsageContract.parse(JSONObject(
        """
        {"v":1,"attempt_id":"a","type":"usage_result","state":"ok",
         "request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-26T09:32:14Z",
         "schema_version":"1.0","timezone":"Europe/Moscow","as_of":"2026-09-26T09:32:13Z",
         "coverage_start":"2026-09-26T09:27:13Z",
         "buckets":[
           {"period":"today","rx_bytes":145776,"tx_bytes":342200,"complete":false},
           {"period":"7d","rx_bytes":145776,"tx_bytes":342200,"complete":false},
           {"period":"30d","rx_bytes":145776,"tx_bytes":342200,"complete":false}]}
        """.trimIndent()), fetchedAt) as UsageEvent.Snapshot).usage

    /** Mirrors the notification rule: the server segment is shown only while Connected. */
    private fun usageSegment(phase: String, state: ServerUsage?, unavailable: Boolean, now: Long): String? =
        if (phase != "Connected") null
        else if (state != null) ServerUsageText.notificationSegment(state, now)
        else if (unavailable) "Зачёт: нет данных" else null

    @Test
    fun `connect to traffic keeps local and server totals distinct`() {
        val sampler = TrafficSampler()
        assertFalse(sampler.onExplicitConnect().measured)
        val first = sampler.onPeerSample(1_000, 500, 1_000, "node-1")
        assertTrue(first.measured)
        assertEquals(1_000L, first.rxTotal)
        val second = sampler.onPeerSample(3_000, 1_500, 2_000, "node-1")
        assertEquals(3_000L, second.rxTotal)
        assertEquals(1_500L, second.txTotal)
        assertTrue(TrafficText.trafficLine(second).contains("2.9 КБ"))

        val account = usage(2_000L)
        val local = "↓${TrafficText.bytes(second.rxTotal)} ↑${TrafficText.bytes(second.txTotal)}"
        val server = usageSegment("Connected", account, false, 2_000L)
        assertTrue(server!!.contains("Зачёт сегодня"))
        assertFalse(local == server)
    }

    @Test
    fun `disconnect keeps last totals, clears the server segment, marks account stale`() {
        val sampler = TrafficSampler()
        sampler.onExplicitConnect()
        sampler.onPeerSample(2_048, 1_024, 1_000, "node-1")
        val off = sampler.onDisconnect()
        assertFalse(off.active)
        assertEquals(2_048L, off.rxTotal)
        assertEquals(0L, off.rxRateBps)
        assertFalse(off.measured)

        val account = usage(1_000L)
        assertTrue(ServerUsageText.isStale(account, 1_000L + ServerUsageText.STALE_MS + 1))
        assertTrue(ServerUsageText.line(account, 1_000_000L, false).contains("устарело"))
        assertNull(usageSegment("Idle", account, false, 2_000L))
    }

    @Test
    fun `unavailable account data is shown as unknown not zero`() {
        assertEquals("Зачёт: нет данных", usageSegment("Connected", null, true, 0L))
        assertNull(usageSegment("Connected", null, false, 0L))
    }
}

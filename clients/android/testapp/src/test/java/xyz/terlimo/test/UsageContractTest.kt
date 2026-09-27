package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

class UsageContractTest {
    private fun okEvent(coverage: String = "\"coverage_start\":\"2026-09-26T09:27:13Z\"",
                        asOf: String = "\"as_of\":\"2026-09-26T09:32:13Z\"",
                        rx: Long = 145776, tx: Long = 342200,
                        todayComplete: Boolean = false): JSONObject = JSONObject(
        """
        {"v":1,"attempt_id":"attempt","type":"usage_result","state":"ok",
         "request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-26T09:32:14Z",
         "schema_version":"1.0","timezone":"Europe/Moscow",$asOf,
         $coverage,
         "buckets":[
           {"period":"today","rx_bytes":$rx,"tx_bytes":$tx,"complete":$todayComplete},
           {"period":"7d","rx_bytes":$rx,"tx_bytes":$tx,"complete":false},
           {"period":"30d","rx_bytes":$rx,"tx_bytes":$tx,"complete":false}]}
        """.trimIndent())

    @Test
    fun `strict ok snapshot keeps all three buckets distinct`() {
        val event = UsageContract.parse(okEvent(), 1_000L)
        assertTrue(event is UsageEvent.Snapshot)
        val usage = (event as UsageEvent.Snapshot).usage
        assertEquals("2026-09-26T09:32:13Z", usage.asOf)
        assertEquals("2026-09-26T09:27:13Z", usage.coverageStart)
        assertEquals(145776L, usage.today.rxBytes)
        assertEquals(342200L, usage.today.txBytes)
        assertFalse(usage.complete)
        assertFalse(usage.stale)
    }

    @Test
    fun `credited arrows are user perspective download tx upload rx`() {
        val usage = (UsageContract.parse(okEvent(rx = 411876, tx = 731256), 1_000L)
            as UsageEvent.Snapshot).usage
        assertEquals(411876L, usage.today.rxBytes)
        assertEquals(731256L, usage.today.txBytes)
        val line = ServerUsageText.line(usage, 1_000L, false)
        assertTrue("line=$line", line.contains("\u2193714.1 \u041a\u0411"))
        assertTrue("line=$line", line.contains("\u2191402.2 \u041a\u0411"))
        val seg = ServerUsageText.notificationSegment(usage, 1_000L)
        assertTrue("seg=$seg", seg.contains("\u2193714.1 \u041a\u0411"))
        assertTrue("seg=$seg", seg.contains("\u2191402.2 \u041a\u0411"))
    }

    @Test
    fun `credited arrows scale MiB for large raw gateway counters`() {
        val usage = (UsageContract.parse(okEvent(rx = 1194680, tx = 8676132), 1_000L)
            as UsageEvent.Snapshot).usage
        val line = ServerUsageText.line(usage, 1_000L, false)
        assertTrue("line=$line", line.contains("\u21938.3 \u041c\u0411"))
        assertTrue("line=$line", line.contains("\u21911.1 \u041c\u0411"))
        assertTrue(ServerUsageText.notificationSegment(usage, 1_000L).contains("\u21938.3 \u041c\u0411"))
    }

    @Test
    fun `null as_of with zero buckets and complete false is a truthful empty history`() {
        val usage = (UsageContract.parse(
            okEvent(coverage = "\"coverage_start\":null", asOf = "\"as_of\":null", rx = 0, tx = 0), 1_000L)
            as UsageEvent.Snapshot).usage
        assertNull(usage.asOf)
        assertNull(usage.coverageStart)
        assertFalse(usage.complete)
        val line = ServerUsageText.line(usage, 1_000L, false)
        assertTrue(line.contains("\u21930 \u0411"))
        assertTrue(line.contains("\u0441\u0431\u043e\u0440 \u0441 \u043d\u0443\u043b\u044f"))
        assertTrue(line.contains("\u0438\u0441\u0442\u043e\u0440\u0438\u044f \u0435\u0449\u0451 \u043d\u0435 \u0441\u043e\u0431\u0440\u0430\u043d\u0430"))
        assertTrue(line.contains("\u043d\u0435\u043f\u043e\u043b\u043d\u043e"))
        assertFalse(line.contains("\u043d\u0435\u0442 \u0434\u0430\u043d\u043d\u044b\u0445"))
    }

    @Test
    fun `complete false and null coverage are shown honestly`() {
        val usage = (UsageContract.parse(okEvent(coverage = "\"coverage_start\":null"), 1_000L)
            as UsageEvent.Snapshot).usage
        assertNull(usage.coverageStart)
        val line = ServerUsageText.line(usage, 1_000L, false)
        assertTrue(line.contains("сбор с нуля"))
        assertTrue(line.contains("неполно"))
        assertTrue(ServerUsageText.notificationSegment(usage, 1_000L).contains("неполно"))
    }

    @Test
    fun `stale snapshot is marked after the freshness window`() {
        val usage = (UsageContract.parse(okEvent(), 1_000L) as UsageEvent.Snapshot).usage
        assertFalse(ServerUsageText.isStale(usage, 1_000L + ServerUsageText.STALE_MS))
        assertTrue(ServerUsageText.isStale(usage, 1_000L + ServerUsageText.STALE_MS + 1))
        assertTrue(ServerUsageText.line(usage, 1_000_000L, false).contains("устарело"))
    }

    @Test
    fun `bounded error event is a Failure and unknown codes are rejected`() {
        val failure = UsageContract.parse(JSONObject(
            """{"v":1,"attempt_id":"a","type":"usage_result","state":"error","code":"SESSION_EXPIRED"}"""), 0L)
        assertEquals("SESSION_EXPIRED", (failure as UsageEvent.Failure).code)
        assertThrows(IllegalStateException::class.java) {
            UsageContract.parse(JSONObject(
                """{"v":1,"attempt_id":"a","type":"usage_result","state":"error","code":"HACK"}"""), 0L)
        }
    }

    @Test
    fun `malformed payloads are rejected, never coerced`() {
        val bad = listOf(
            okEvent().put("extra", 1),
            okEvent(coverage = "\"coverage_start\":\"bad\""),
            JSONObject(
                """{"v":1,"attempt_id":"a","type":"usage_result","state":"ok","request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-26T09:32:14Z","schema_version":"1.0","timezone":"UTC","as_of":"2026-09-26T09:32:13Z","coverage_start":null,"buckets":[]}"""),
            JSONObject(
                """{"v":1,"attempt_id":"a","type":"usage_result","state":"ok","request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-26T09:32:14Z","schema_version":"1.0","timezone":"Europe/Moscow","as_of":"2026-09-26T09:32:13Z","coverage_start":null,"buckets":[{"period":"today","rx_bytes":-1,"tx_bytes":1,"complete":false},{"period":"7d","rx_bytes":1,"tx_bytes":1,"complete":false},{"period":"30d","rx_bytes":1,"tx_bytes":1,"complete":false}]}"""),
        )
        for (event in bad) {
            assertThrows(IllegalStateException::class.java) { UsageContract.parse(event, 0L) }
        }
    }

    @Test
    fun `public usage_result codes stay the host-accepted transport set`() {
        for (code in listOf("TRANSPORT", "MOBILE_STATE_UNAVAILABLE")) {
            val event = UsageContract.parse(JSONObject(
                """{"v":1,"attempt_id":"a","type":"usage_result","state":"error","code":"$code"}"""), 0L)
            assertEquals(code, (event as UsageEvent.Failure).code)
        }
        // The new diagnostic FAIL_* tokens are stderr-only and never a public usage_result code.
        for (code in listOf("FAIL_LOCAL_PATH_REJECTED", "FAIL_SERVICE_PATH_DENIED", "FAIL_TRANSPORT")) {
            assertThrows(IllegalStateException::class.java) {
                UsageContract.parse(JSONObject(
                    """{"v":1,"attempt_id":"a","type":"usage_result","state":"error","code":"$code"}"""), 0L)
            }
        }
    }

    @Test
    fun `unknown account totals are unknown, not zero`() {
        assertEquals("Зачёт: —", ServerUsageText.line(null, 0L, false))
        assertEquals("Зачёт: нет данных", ServerUsageText.line(null, 0L, true))
    }
}

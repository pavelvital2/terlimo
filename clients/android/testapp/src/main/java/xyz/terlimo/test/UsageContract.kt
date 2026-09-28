package xyz.terlimo.test

import java.util.Locale
import org.json.JSONArray
import org.json.JSONObject

/**
 * S5 §07.4 host projection of the native account-traffic bridge vocabulary
 * (go_client/terlimo_usage.go). This is a strict read of the `usage_result` event: the
 * accepted key set, required fields and bucket enums are enforced, no field is invented
 * and a malformed event is rejected while the last good projection is kept.
 *
 * Server credited totals (today/7d/30d) are account truth and are always kept strictly
 * distinct from the local live tunnel RX/TX/rate of [TrafficAccounting]. `complete=false`
 * and a null `coverage_start` are shown honestly, never coerced into a finished value.
 */
internal data class UsageBucket(
    val period: String,
    val rxBytes: Long,
    val txBytes: Long,
    val complete: Boolean,
)

internal data class ServerUsage(
    /** Null when the server has no trustworthy sample yet; never replaced by server_time. */
    val asOf: String?,
    val coverageStart: String?,
    val timezone: String,
    val today: UsageBucket,
    val sevenDay: UsageBucket,
    val thirtyDay: UsageBucket,
    val fetchedAtElapsedMs: Long,
    /** Last known totals are kept but marked stale after a failed/expired refresh or Disconnect. */
    val stale: Boolean = false,
) {
    val complete: Boolean get() = today.complete && sevenDay.complete && thirtyDay.complete
}

internal sealed class UsageEvent {
    data class Snapshot(val usage: ServerUsage) : UsageEvent()
    data class Failure(val code: String) : UsageEvent()
}

internal object UsageContract {
    const val TYPE_USAGE_RESULT = "usage_result"
    const val ACTION_USAGE_READ = "usage_read"

    /** Bounded native classification codes plus the frozen schemas/errors.json enum. */
    val ERROR_CODES: Set<String> = PaymentsContract.ERROR_CODES

    private val PERIODS = setOf("today", "7d", "30d")
    private val ENVELOPE = setOf("v", "attempt_id", "type", "state")
    private val OK_KEYS = ENVELOPE + setOf(
        "request_id", "server_time", "schema_version", "timezone", "as_of",
        "coverage_start", "buckets",
    )
    private val ERROR_KEYS = ENVELOPE + setOf("code")
    private val BUCKET_KEYS = setOf("period", "rx_bytes", "tx_bytes", "complete")
    private val REQUEST_ID = Regex("^[0-9a-f]{32}$")
    private val UTC_TIME = Regex("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$")
    private const val MAX_BYTES = 9_007_199_254_740_991L

    fun parse(event: JSONObject, fetchedAtElapsedMs: Long): UsageEvent {
        try {
            val type = event.getString("type")
            check(type == TYPE_USAGE_RESULT) { "USAGE_INVALID" }
            val state = event.getString("state")
            val keys = event.keys().asSequence().toSet()
            if (state == "error") {
                check(keys == ERROR_KEYS) { "USAGE_INVALID" }
                val code = event.getString("code")
                check(code in ERROR_CODES) { "USAGE_INVALID" }
                return UsageEvent.Failure(code)
            }
            check(state == "ok" && keys == OK_KEYS) { "USAGE_INVALID" }
            check(REQUEST_ID.matches(event.getString("request_id"))) { "USAGE_INVALID" }
            check(UTC_TIME.matches(event.getString("server_time"))) { "USAGE_INVALID" }
            check(event.getString("schema_version") == "1.0") { "USAGE_INVALID" }
            val timezone = event.getString("timezone")
            check(timezone == "Europe/Moscow") { "USAGE_INVALID" }
            val asOf = if (event.isNull("as_of")) null else
                event.getString("as_of").takeIf { UTC_TIME.matches(it) }
            if (!event.isNull("as_of")) check(asOf != null) { "USAGE_INVALID" }
            val coverageStart = if (event.isNull("coverage_start")) null else
                event.getString("coverage_start").takeIf { UTC_TIME.matches(it) }
            if (!event.isNull("coverage_start")) check(coverageStart != null) { "USAGE_INVALID" }
            val buckets = parseBuckets(event.getJSONArray("buckets"))
            return UsageEvent.Snapshot(
                ServerUsage(asOf, coverageStart, timezone, buckets.getValue("today"),
                    buckets.getValue("7d"), buckets.getValue("30d"), fetchedAtElapsedMs),
            )
        } catch (error: IllegalStateException) {
            throw error
        } catch (error: Exception) {
            throw IllegalStateException("USAGE_INVALID", error)
        }
    }

    private fun parseBuckets(array: JSONArray): Map<String, UsageBucket> {
        check(array.length() == 3) { "USAGE_INVALID" }
        val buckets = LinkedHashMap<String, UsageBucket>()
        for (index in 0 until array.length()) {
            val bucket = array.getJSONObject(index)
            check(bucket.keys().asSequence().toSet() == BUCKET_KEYS) { "USAGE_INVALID" }
            val period = bucket.getString("period")
            check(period in PERIODS && buckets[period] == null) { "USAGE_INVALID" }
            val rx = bucket.getLong("rx_bytes")
            val tx = bucket.getLong("tx_bytes")
            check(rx in 0..MAX_BYTES && tx in 0..MAX_BYTES) { "USAGE_INVALID" }
            buckets[period] = UsageBucket(period, rx, tx, bucket.getBoolean("complete"))
        }
        check(buckets.keys == PERIODS) { "USAGE_INVALID" }
        return buckets
    }
}

/**
 * Display-only formatting of the account credited totals, kept explicitly distinct from the
 * local live [TrafficText] line. Unknown/stale/gap and `complete=false` are shown, never hidden.
 */
internal object ServerUsageText {
    const val STALE_MS = 90_000L

    fun isStale(usage: ServerUsage, nowElapsedMs: Long): Boolean =
        usage.stale || usage.fetchedAtElapsedMs <= 0 || nowElapsedMs - usage.fetchedAtElapsedMs > STALE_MS

    // Product arrows are subscriber-perspective while the DTO keeps raw gateway-side peer
    // counters: DOWNLOAD (↓) = API tx_bytes, UPLOAD (↑) = API rx_bytes. Local session
    // speed/traffic counters (TrafficText) are NOT swapped — only the server credited «Зачёт».
    fun bucketLine(bucket: UsageBucket): String =
        "↓${TrafficText.bytes(bucket.txBytes)} ↑${TrafficText.bytes(bucket.rxBytes)}"

    /** Compact one-line account total used by the notification (today only). */
    fun notificationSegment(usage: ServerUsage, nowElapsedMs: Long): String {
        val suffix = buildList {
            if (!usage.complete) add("неполно")
            if (usage.asOf == null) add("история ещё не собрана")
            if (isStale(usage, nowElapsedMs)) add("устарело")
        }.joinToString(",")
        val prefix = if (usage.coverageStart == null) "сбор с нуля" else "с ${shortTime(usage.coverageStart)}"
        val tail = if (suffix.isEmpty()) prefix else "$prefix, $suffix"
        return "Зачёт сегодня ${bucketLine(usage.today)} ($tail)"
    }

    /** Full distinct account line with all three windows. */
    fun line(usage: ServerUsage?, nowElapsedMs: Long, unavailable: Boolean): String {
        if (usage == null) return if (unavailable) "Зачёт: нет данных" else "Зачёт: —"
        val suffix = buildList {
            if (!usage.complete) add("неполно")
            if (usage.asOf == null) add("история ещё не собрана")
            if (isStale(usage, nowElapsedMs)) add("устарело")
        }.joinToString(", ")
        val coverage = if (usage.coverageStart == null) "сбор с нуля" else "с ${shortTime(usage.coverageStart)}"
        val tail = if (suffix.isEmpty()) coverage else "$coverage · $suffix"
        return "Зачёт: сегодня ${bucketLine(usage.today)} · 7д ${bucketLine(usage.sevenDay)} · " +
            "30д ${bucketLine(usage.thirtyDay)} ($tail)"
    }

    /**
     * §20 account total traffic for the Subscription tab: the same server buckets, all
     * devices, no per-device split and no local summing. A null coverage with nonzero
     * historical totals is shown as partial history, never as "from scratch".
     */
    fun accountTraffic(usage: ServerUsage?, nowElapsedMs: Long, unavailable: Boolean): String {
        if (usage == null) return if (unavailable) "Трафик аккаунта: нет данных" else "Трафик аккаунта: —"
        fun line(label: String, bucket: UsageBucket): String {
            val total = bucket.rxBytes + bucket.txBytes
            return "$label: ${bucketLine(bucket)} · всего ${TrafficText.bytes(total)}"
        }
        val historical = listOf(usage.today, usage.sevenDay, usage.thirtyDay)
            .any { it.rxBytes > 0 || it.txBytes > 0 }
        val coverage = when {
            usage.coverageStart != null -> "Покрытие с ${shortTime(usage.coverageStart)}"
            historical -> "Покрытие: частичная история"
            else -> "Покрытие: история ещё не собрана"
        }
        val suffix = buildList {
            if (!usage.complete) add("неполно")
            if (isStale(usage, nowElapsedMs)) add("устарело")
        }
        val tail = if (suffix.isEmpty()) coverage else "$coverage · ${suffix.joinToString(", ")}"
        return "Трафик аккаунта (все устройства)\n" +
            line("Сегодня", usage.today) + "\n" +
            line("7 дней", usage.sevenDay) + "\n" +
            line("30 дней", usage.thirtyDay) + "\n" +
            serverTimeLine(usage.asOf) + "\n" + tail
    }

    /**
     * §20 data time: the SERVER `as_of` of this snapshot, never the local fetch time.
     * A null server time is stated as unknown; the totals themselves stay untouched.
     */
    fun serverTimeLine(asOf: String?): String {
        val value = asOf ?: return "Время данных неизвестно"
        val formatted = runCatching {
            java.time.Instant.parse(value)
                .atZone(java.time.ZoneId.of("Europe/Moscow"))
                .format(java.time.format.DateTimeFormatter.ofPattern("dd.MM.yyyy HH:mm:ss", java.util.Locale.ROOT))
        }.getOrNull() ?: return "Время данных неизвестно"
        return "Данные на $formatted (Europe/Moscow)"
    }

    private fun shortTime(utc: String): String = runCatching {
        val time = utc.substringAfter("T").take(5)
        String.format(Locale.ROOT, "%s UTC", time)
    }.getOrDefault("—")
}

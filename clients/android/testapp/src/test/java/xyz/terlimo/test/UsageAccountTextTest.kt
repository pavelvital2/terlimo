package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class UsageAccountTextTest {
    private fun bucket(period: String, rx: Long, tx: Long, complete: Boolean = false) =
        UsageBucket(period, rx, tx, complete)

    private fun usage(todayRx: Long = 0, todayTx: Long = 9812,
        weekRx: Long = 5_534_388, weekTx: Long = 149_643_712,
        coverage: String? = null, stale: Boolean = false) = ServerUsage(
        asOf = "2026-09-27T17:05:53Z",
        coverageStart = coverage,
        timezone = "Europe/Moscow",
        today = bucket("today", todayRx, todayTx),
        sevenDay = bucket("7d", weekRx, weekTx),
        thirtyDay = bucket("30d", weekRx, weekTx),
        fetchedAtElapsedMs = 1_000,
        stale = stale,
    )

    @Test fun accountBucketsRenderWithServerDirectionsUnitsAndTotals() {
        val text = ServerUsageText.accountTraffic(usage(), nowElapsedMs = 2_000, unavailable = false)
        assertTrue(text.startsWith("Трафик аккаунта (все устройства)"))
        val up = TrafficText.bytes(5_534_388)
        val down = TrafficText.bytes(149_643_712)
        assertTrue(text.contains("Сегодня: ↓${TrafficText.bytes(9_812)} ↑${TrafficText.bytes(0)}"))
        assertTrue(text.contains("7 дней: ↓$down ↑$up · всего ${TrafficText.bytes(5_534_388L + 149_643_712L)}"))
        assertTrue(text.contains("30 дней: ↓$down ↑$up"))
    }

    @Test fun partialAndStaleAreHonestAndNullCoverageIsNotFromScratch() {
        val partial = ServerUsageText.accountTraffic(usage(), 2_000, false)
        assertTrue(partial.contains("Покрытие: частичная история"))
        assertFalse(partial.contains("сбор с нуля"))
        assertTrue(partial.contains("неполно"))
        val stale = ServerUsageText.accountTraffic(usage(stale = true), 2_000, false)
        assertTrue(stale.contains("устарело"))
        assertTrue(stale.contains("Покрытие: частичная история"))
        val zero = ServerUsageText.accountTraffic(
            usage(todayRx = 0, todayTx = 0, weekRx = 0, weekTx = 0), 2_000, false)
        assertTrue(zero.contains("Покрытие: история ещё не собрана"))
        val covered = ServerUsageText.accountTraffic(
            usage(coverage = "2026-09-27T17:00:00Z"), 2_000, false)
        assertTrue(covered.contains("Покрытие с 17:00 UTC"))
    }

    @Test fun dataTimeIsTheServerAsOfNotTheLocalFetchTime() {
        val text = ServerUsageText.accountTraffic(usage(), nowElapsedMs = 999_000, unavailable = false)
        // as_of 17:05:53Z displayed in the DTO timezone; fetch time never appears.
        assertTrue(text.contains("Данные на 27.09.2026 20:05:53 (Europe/Moscow)"))
        val unknown = ServerUsageText.accountTraffic(
            usage().copy(asOf = null), 2_000, false)
        assertTrue(unknown.contains("Время данных неизвестно"))
        assertTrue(unknown.contains("Сегодня: ↓${TrafficText.bytes(9_812)}"))
    }

    @Test fun missingUsageIsStatedWithoutInventedZeroes() {
        assertTrue(ServerUsageText.accountTraffic(null, 2_000, unavailable = true)
            .contains("нет данных"))
        assertTrue(ServerUsageText.accountTraffic(null, 2_000, unavailable = false).contains("—"))
    }
}

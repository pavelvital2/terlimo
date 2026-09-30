package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/** §26.5 schedule policy: one periodic entry, Off by default, no catch-up. */
class CatalogRefreshPolicyTest {

    @Test fun intervalsMatchTheApprovedModes() {
        assertNull(CatalogRefreshPolicy.intervalMillis(CatalogRefreshMode.OFF))
        assertEquals(12L * 60 * 60 * 1000, CatalogRefreshPolicy.intervalMillis(CatalogRefreshMode.TWICE_DAILY))
        assertEquals(24L * 60 * 60 * 1000, CatalogRefreshPolicy.intervalMillis(CatalogRefreshMode.DAILY))
        assertEquals(7L * 24 * 60 * 60 * 1000, CatalogRefreshPolicy.intervalMillis(CatalogRefreshMode.WEEKLY))
    }

    @Test fun storedModeDefaultsToOff() {
        assertEquals(CatalogRefreshMode.OFF, CatalogRefreshMode.fromStored(null))
        assertEquals(CatalogRefreshMode.OFF, CatalogRefreshMode.fromStored("garbage"))
        assertEquals(CatalogRefreshMode.DAILY, CatalogRefreshMode.fromStored("daily"))
        assertEquals(CatalogRefreshMode.WEEKLY, CatalogRefreshMode.fromStored("weekly"))
    }

    @Test fun reconcileKeepsAnIdenticalPeriodAndReplacesAChangedOne() {
        assertEquals(
            CatalogRefreshPolicy.Decision.UNCHANGED,
            CatalogRefreshPolicy.reconcile(12L * 60 * 60 * 1000, CatalogRefreshMode.TWICE_DAILY),
        )
        assertEquals(
            CatalogRefreshPolicy.Decision.UNCHANGED,
            CatalogRefreshPolicy.reconcile(24L * 60 * 60 * 1000, CatalogRefreshMode.DAILY),
        )
        assertEquals(
            CatalogRefreshPolicy.Decision.SCHEDULE,
            CatalogRefreshPolicy.reconcile(12L * 60 * 60 * 1000, CatalogRefreshMode.DAILY),
        )
        assertEquals(
            CatalogRefreshPolicy.Decision.SCHEDULE,
            CatalogRefreshPolicy.reconcile(null, CatalogRefreshMode.WEEKLY),
        )
        assertEquals(
            CatalogRefreshPolicy.Decision.CANCEL,
            CatalogRefreshPolicy.reconcile(12L * 60 * 60 * 1000, CatalogRefreshMode.OFF),
        )
        assertEquals(
            CatalogRefreshPolicy.Decision.CANCEL,
            CatalogRefreshPolicy.reconcile(null, CatalogRefreshMode.OFF),
        )
    }

    @Test fun uiLabelsAndStatusAreHonestForEveryMode() {
        assertEquals("Выключено", CatalogRefreshPolicy.label(CatalogRefreshMode.OFF))
        assertEquals("2 раза в день", CatalogRefreshPolicy.label(CatalogRefreshMode.TWICE_DAILY))
        assertEquals("Ежедневно", CatalogRefreshPolicy.label(CatalogRefreshMode.DAILY))
        assertEquals("Раз в неделю", CatalogRefreshPolicy.label(CatalogRefreshMode.WEEKLY))
        // Off is explicit; a refused schedule is never presented as active; periods are approximate.
        assertEquals("Обновление каталога выключено.",
            CatalogRefreshPolicy.statusText(CatalogRefreshMode.OFF, refused = false))
        assertEquals("Обновление каталога: Ежедневно (примерно).",
            CatalogRefreshPolicy.statusText(CatalogRefreshMode.DAILY, refused = false))
        val refused = CatalogRefreshPolicy.statusText(CatalogRefreshMode.WEEKLY, refused = true)
        assertTrue(refused.contains("Не удалось"))
        assertFalse(refused.contains("Раз в неделю"))
    }

    @Test fun refusedMappingCoversStartupChangeRecreateAndRetry() {
        // Startup with a stored DAILY that the platform refuses -> refused (never shown active).
        assertTrue(CatalogRefreshPolicy.refused(
            CatalogRefreshMode.DAILY, CatalogRefreshPolicy.Decision.SCHEDULE, scheduleResult = 0))
        // Accepted schedule is not refused...
        assertFalse(CatalogRefreshPolicy.refused(
            CatalogRefreshMode.DAILY, CatalogRefreshPolicy.Decision.SCHEDULE, CatalogRefreshPolicy.SCHEDULE_SUCCESS))
        // ...and neither is an idempotent recreate that found the matching entry (UNCHANGED).
        assertFalse(CatalogRefreshPolicy.refused(
            CatalogRefreshMode.DAILY, CatalogRefreshPolicy.Decision.UNCHANGED, null))
        // Off is never a refusal, even when the cancel call threw.
        assertFalse(CatalogRefreshPolicy.refused(
            CatalogRefreshMode.OFF, CatalogRefreshPolicy.Decision.CANCEL, null, threw = true))
        // A scheduler throw while a period is requested is a refusal (no false success)...
        assertTrue(CatalogRefreshPolicy.refused(
            CatalogRefreshMode.WEEKLY, CatalogRefreshPolicy.Decision.SCHEDULE, null, threw = true))
        // ...and a retry that then succeeds clears it.
        assertFalse(CatalogRefreshPolicy.refused(
            CatalogRefreshMode.WEEKLY, CatalogRefreshPolicy.Decision.SCHEDULE, CatalogRefreshPolicy.SCHEDULE_SUCCESS))
    }
}

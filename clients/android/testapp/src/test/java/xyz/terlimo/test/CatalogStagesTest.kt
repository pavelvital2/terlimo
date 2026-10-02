package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class CatalogStagesTest {
    @Test fun slowConnectionDoesNotSpendDeviceBudget() {
        val stages = CatalogStages()
        stages.begin("a", "cycle", 0)
        assertTrue(stages.advance("a", "cycle", CatalogStage.DEVICE, 19_000))
        assertEquals(25_000L, stages.remaining(19_000))
        assertEquals(1L, stages.remaining(43_999))
    }
    @Test fun realReconnectResumesOnlyUnusedActiveBudgets() {
        val stages = CatalogStages(); stages.begin("a", "one", 0)
        assertFalse(stages.advance("a", "one", CatalogStage.CONNECTING, 1_000))
        assertTrue(stages.advance("a", "one", CatalogStage.DEVICE, 5_000))
        assertTrue(stages.advance("a", "one", CatalogStage.CONNECTING, 10_000))
        assertEquals(CatalogStage.CONNECTING, stages.stage)
        assertEquals(15_000L, stages.remaining(10_000))
        assertTrue(stages.advance("a", "one", CatalogStage.DEVICE, 12_000))
        assertEquals(20_000L, stages.remaining(12_000))
        assertFalse(stages.advance("a", "one", CatalogStage.DEVICE, 15_000))
        assertEquals(0L, stages.remaining(32_000))
    }
    @Test fun observedAuthFailureDoesNotSpendConnectionBudgetWhileWaitingForDevice() {
        val stages = CatalogStages(); stages.begin("a", "one", 0)
        assertTrue(stages.advance("a", "one", CatalogStage.DEVICE, 3_000))
        assertTrue(stages.advance("a", "one", CatalogStage.CONNECTING, 19_000))
        assertEquals(17_000L, stages.remaining(19_000))
        assertTrue(stages.canAccept("a", "one", 20_000))
        assertFalse(stages.advance("a", "one", CatalogStage.CONNECTING, 20_000))
        assertEquals(16_000L, stages.remaining(20_000))
        assertTrue(stages.advance("a", "one", CatalogStage.DEVICE, 21_000))
        assertEquals(9_000L, stages.remaining(21_000))
        assertFalse(stages.advance("a", "one", CatalogStage.SUBSCRIPTION, 30_000))
        assertFalse(stages.canAccept("a", "one", 30_000))
    }
    @Test fun matchingCatalogAfterDeadlineIsRejectedBeforeDelayedCallback() {
        var clock = 0L; var called = false
        val timer = CatalogDeadlineTimer({ _, _ -> }, { "a" }, { called = true }, elapsed = { clock })
        timer.beginStages("a", "one")
        assertTrue(timer.canAccept("a", "one"))
        clock = 20_000
        assertFalse(timer.canAccept("a", "one"))
        assertFalse(called)
        assertTrue(timer.pending("a"))
    }
    @Test fun expiredStageCannotBeRevivedByNextEvent() {
        val stages = CatalogStages(); stages.begin("a", "one", 0)
        assertFalse(stages.advance("a", "one", CatalogStage.DEVICE, 20_000))
        assertEquals(0L, stages.remaining(20_000))
    }
    @Test fun oldOperationAndOldAttemptCannotAdvanceNewRefresh() {
        val stages = CatalogStages(); stages.begin("a", "new", 100)
        assertFalse(stages.advance("a", "old", CatalogStage.LIST, 200))
        assertFalse(stages.advance("old", "new", CatalogStage.LIST, 200))
        assertEquals(CatalogStage.CONNECTING, stages.stage)
        stages.clear()
        assertFalse(stages.advance("a", "new", CatalogStage.LIST, 300))
    }
    @Test fun allStagesRemainWithinOverallDeadlineAndCannotBeExtended() {
        val stages = CatalogStages(); stages.begin("a", "one", 0)
        assertTrue(stages.advance("a", "one", CatalogStage.DEVICE, 19_999))
        assertTrue(stages.advance("a", "one", CatalogStage.SUBSCRIPTION, 44_998))
        assertTrue(stages.advance("a", "one", CatalogStage.LIST, 54_997))
        assertEquals(10_000L, stages.remaining(54_997))
        assertEquals(0L, stages.remaining(65_000))
        assertFalse(stages.advance("a", "one", CatalogStage.LIST, 65_001))
    }
    @Test fun eachErrorIdentifiesActualStageAndManualRetryHasNewFence() {
        for (stage in CatalogStage.entries) {
            assertTrue(UserStatusText.error("CATALOG_${stage.name}_TIMEOUT")!!.contains(stage.label))
        }
        val stages = CatalogStages(); stages.begin("a", "failed", 0)
        stages.clear(); stages.begin("a", "retry", 50_000)
        assertEquals(20_000L, stages.remaining(50_000))
        assertFalse(stages.advance("a", "failed", CatalogStage.LIST, 50_001))
    }
    @Test fun reusedSessionCanSkipConnectionAndAuthenticationWithoutInventedProgress() {
        val stages = CatalogStages(); stages.begin("a", "one", 0)
        assertTrue(stages.advance("a", "one", CatalogStage.SUBSCRIPTION, 10))
        assertEquals(CatalogStage.SUBSCRIPTION, stages.stage)
        assertEquals(10_000L, stages.remaining(10))
    }
    @Test fun hostTimerFencesSupersededCallbacksAndStopsOnlyActualStage() {
        var clock = 0L; var active: String? = "a"
        val callbacks = mutableListOf<() -> Unit>(); val delays = mutableListOf<Long>()
        val failures = mutableListOf<CatalogStage>()
        val timer = CatalogDeadlineTimer({ d, cb -> delays += d; callbacks += cb }, { active }, {},
            elapsed = { clock }, onStageTimeout = { _, stage -> failures += stage })
        timer.beginStages("a", "one")
        clock = 19_000
        assertTrue(timer.advanceStage("a", "one", CatalogStage.DEVICE))
        assertEquals(25_000L, delays.last())
        callbacks.first()(); assertTrue(failures.isEmpty())
        clock = 44_000; callbacks.last()()
        assertEquals(listOf(CatalogStage.DEVICE), failures)
        assertFalse(timer.pending("a"))
        active = "b"; callbacks.last()(); assertEquals(1, failures.size)
    }
    @Test fun acceptedBrowseDisarmsAndCancelledAttemptCannotTimeout() {
        var clock = 0L; val callbacks = mutableListOf<() -> Unit>(); var failed = false
        val timer = CatalogDeadlineTimer({ _, cb -> callbacks += cb }, { "a" }, { failed = true }, elapsed = { clock })
        timer.beginStages("a", "one")
        timer.apply("a", MobileCatalogAction.DISARM, CatalogTimerMarker.ACCEPT)
        clock = 65_000; callbacks.forEach { it() }; assertFalse(failed)
        timer.beginStages("a", "two"); timer.clear()
        callbacks.forEach { it() }; assertFalse(failed)
    }
}

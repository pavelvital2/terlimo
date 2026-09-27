package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class SleepRetentionPolicyTest {
    @Test fun defaultDelayedPauseWaitsFiveMinutesAndOnlyWakeResumes() {
        val cycle = SleepRetentionPolicy.begin(3, 1_000, SleepRetentionPolicy.DEFAULT_PAUSE_MINUTES, null)!!
        assertFalse(SleepRetentionPolicy.pauseDue(cycle, 3, 300_999, false, false, true))
        assertTrue(SleepRetentionPolicy.pauseDue(cycle, 3, 301_000, false, false, true))
        assertFalse(SleepRetentionPolicy.resumeDue(cycle, 3, 999_000, false, false, true))
        assertTrue(SleepRetentionPolicy.resumeDue(cycle, 3, 999_000, true, false, true))
    }
    @Test fun oldCycleAndUserStopCannotPauseOrResume() {
        val cycle = SleepRetentionPolicy.begin(3, 1_000, 0, 1)!!
        assertFalse(SleepRetentionPolicy.pauseDue(cycle, 4, 50_000, false, false, true))
        assertFalse(SleepRetentionPolicy.pauseDue(cycle, 3, 50_000, true, false, true))
        assertFalse(SleepRetentionPolicy.pauseDue(cycle, 3, 50_000, false, true, true))
        assertFalse(SleepRetentionPolicy.resumeDue(cycle, 4, 70_000, false, false, true))
        assertFalse(SleepRetentionPolicy.resumeDue(cycle, 3, 70_000, false, true, true))
    }
    @Test fun timedPauseAndZeroTimerMatchV18() {
        assertNull(SleepRetentionPolicy.begin(1, 1_000, 5, 0))
        val cycle = SleepRetentionPolicy.begin(1, 1_000, 5, 1)!!
        assertTrue(SleepRetentionPolicy.pauseDue(cycle, 1, 1_000, false, false, true))
        assertFalse(SleepRetentionPolicy.resumeDue(cycle, 1, 60_999, false, false, true))
        assertTrue(SleepRetentionPolicy.resumeDue(cycle, 1, 61_000, false, false, true))
    }
    @Test fun shortGuardNeverBecomesSessionLongRetention() {
        assertEquals(130_000L, SleepRetentionPolicy.shortGuardMillis(120_000, 0))
        assertNull(SleepRetentionPolicy.shortGuardMillis(300_000, 0))
        assertNull(SleepRetentionPolicy.shortGuardMillis(0, 0))
    }
}

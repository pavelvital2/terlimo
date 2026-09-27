package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class WakeRecoveryProjectionTest {
    @Test fun queuedSameGenerationCannotRestoreReadinessAfterSleep() {
        val queued = WakeRecoveryStatus(2, 3, 8, 36, 36)
        assertNull(WakeRecoveryProjection.accept(null, queued, "a", "a", 2, 4, false, false))
        assertNull(WakeRecoveryProjection.accept(null, queued, "a", "a", 2, 5, true, false))
        val fresh = WakeRecoveryStatus(2, 5, 9, 8, 36)
        assertSame(fresh, WakeRecoveryProjection.accept(null, fresh, "a", "a", 2, 5, true, false))
    }
    @Test fun newRuntimeGenerationOneIsValidAndLateOldRuntimeNineIsNot() {
        val old = WakeRecoveryStatus(2, 3, 8, 36, 36)
        val fresh = WakeRecoveryStatus(3, 3, 1, 12, 36)
        val result = WakeRecoveryProjection.accept(old, fresh, "a", "a", 3, 3, true, false)
        assertSame(fresh, result)
        assertSame(fresh, WakeRecoveryProjection.accept(result, WakeRecoveryStatus(2, 3, 9, 36, 36), "a", "a", 3, 3, true, false))
    }
    @Test fun stopSwitchInvalidationAndWrongAttemptCannotPublishStatus() {
        val queued = WakeRecoveryStatus(2, 3, 8, 36, 36)
        assertNull(WakeRecoveryProjection.accept(null, queued, null, "a", 2, 3, true, false))
        assertNull(WakeRecoveryProjection.accept(null, queued, "b", "a", 2, 3, true, false))
        assertNull(WakeRecoveryProjection.accept(null, queued, "a", "a", 0, 3, true, false))
        assertNull(WakeRecoveryProjection.accept(null, queued, "a", "a", 2, 3, true, true))
    }
    @Test fun malformedTelemetryIsRejectedWithoutSessionEffect() {
        fun valid() = JSONObject().put("v", 1).put("attempt_id", "a").put("type", "wake_status")
            .put("runtime_epoch", 2).put("lifecycle_revision", 3).put("generation", 8).put("ready", 12).put("total", 36)
        assertNotNull(WakeRecoveryProjection.parse(valid()))
        for (bad in listOf(valid().put("ready", "12"), valid().put("runtime_epoch", 2.0),
            valid().put("lifecycle_revision", true), valid().put("generation", MAX_WAKE_INTEGER + 1),
            valid().put("total", 37), valid().put("ready", 37), valid().put("extra", 1)))
            assertNull(WakeRecoveryProjection.parse(bad))
    }
}

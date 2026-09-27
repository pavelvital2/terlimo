package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class ChannelsStatusTest {
    private fun valid() = JSONObject().put("v", 1).put("attempt_id", "a").put("type", "channels_status")
        .put("runtime_epoch", 2).put("lifecycle_revision", 3).put("generation", 0).put("active", 12).put("target", 36)

    @Test fun fullAndHonestZeroAreParsedWithClearText() {
        val full = ChannelsStatusProjection.parse(valid())!!
        assertEquals(12, full.active)
        assertEquals(36, full.target)
        assertEquals("Каналы: 12 из 36", full.text)
        val zero = ChannelsStatusProjection.parse(valid().put("active", 0))!!
        assertEquals("Каналы: 0 из 36", zero.text)
        val retired = ChannelsStatusProjection.parse(valid().put("active", 36).put("target", 36))!!
        assertEquals("Каналы: 36 из 36", retired.text)
    }

    @Test fun unknownTargetIsNotRenderedAsZero() {
        val unknown = ChannelsStatusProjection.parse(valid().put("active", 0).put("target", 0))!!
        assertNull(unknown.text)
        assertNull(ChannelsDisplay.line(true, unknown, null))
        assertNull(ChannelsDisplay.line(false, ChannelsStatusProjection.parse(valid())!!, null))
    }

    @Test fun defaultLifecycleRevisionZeroIsAcceptedWhenConnected() {
        val zero = ChannelsStatusProjection.parse(valid().put("lifecycle_revision", 0))!!
        assertEquals(0L, zero.lifecycleRevision)
        assertSame(zero, ChannelsStatusProjection.accept(null, zero, "a", "a", 2, 0, true, false))
        assertNull(ChannelsStatusProjection.accept(null, zero, "a", "a", 2, 1, true, false))
        assertNull(ChannelsStatusProjection.parse(valid().put("lifecycle_revision", -1)))
    }

    @Test fun displayPrefersWakeReadyDuringWakeAndChannelsOnlyInNormalConnected() {
        val channels = ChannelsStatusProjection.parse(valid())!!
        val wake = WakeRecoveryStatus(runtimeEpoch = 2, lifecycleRevision = 3, generation = 9, ready = 5, total = 7)
        assertEquals("Каналы: 12 из 36", ChannelsDisplay.line(true, channels, null))
        assertEquals("Готовые каналы: 5 из 36", ChannelsDisplay.line(true, channels, wake))
        assertEquals("Готовые каналы: 5 из 24", ChannelsDisplay.line(true, ChannelsStatusProjection.parse(valid().put("target", 24))!!, wake))
        assertEquals("Проверяем каналы: 0 из 36", ChannelsDisplay.line(true, channels, WakeRecoveryStatus(2, 3, 9, 0, 7)))
        assertEquals("Готовые каналы: 5 из 36", ChannelsDisplay.line(true, channels, WakeRecoveryStatus(2, 3, 9, 5, 9)))
        assertEquals("Готовые каналы: 5", ChannelsDisplay.line(true, null, wake))
        assertEquals("Проверяем каналы: 0", ChannelsDisplay.line(true, null, WakeRecoveryStatus(2, 3, 9, 0, 7)))
        assertNull(ChannelsDisplay.line(false, channels, null))
        assertNull(ChannelsDisplay.line(false, null, null))
    }

    @Test fun foreignAttemptOrEpochCannotReplaceTheConfirmedTarget() {
        val current = ChannelsStatusProjection.parse(valid().put("target", 24).put("active", 5))!!
        val late = ChannelsStatusProjection.parse(valid().put("target", 36))!!
        assertSame(current, ChannelsStatusProjection.accept(current, late, "a", "b", 2, 3, true, false))
        assertSame(current, ChannelsStatusProjection.accept(current, late, "a", "a", 5, 3, true, false))
        val wake = WakeRecoveryStatus(2, 3, 9, 5, 7)
        assertEquals("Готовые каналы: 5 из 24", ChannelsDisplay.line(true, current, wake))
    }

    @Test fun lossAndRecoveryWithinTheSameGenerationUpdateTheFact() {
        val first = ChannelsStatusProjection.parse(valid())!!
        val loss = ChannelsStatusProjection.parse(valid().put("active", 8))!!
        assertSame(loss, ChannelsStatusProjection.accept(first, loss, "a", "a", 2, 3, true, false))
        val recovered = ChannelsStatusProjection.parse(valid().put("active", 36))!!
        assertSame(recovered, ChannelsStatusProjection.accept(loss, recovered, "a", "a", 2, 3, true, false))
    }

    @Test fun lateOldGenerationAndWrongAttemptCannotChangeNewState() {
        val current = ChannelsStatus(runtimeEpoch = 2, lifecycleRevision = 3, generation = 9, active = 36, target = 36)
        val late = ChannelsStatusProjection.parse(valid().put("generation", 8).put("active", 0))!!
        assertSame(current, ChannelsStatusProjection.accept(current, late, "a", "a", 2, 3, true, false))
        val newEpoch = ChannelsStatusProjection.parse(valid().put("runtime_epoch", 4))!!
        assertSame(newEpoch, ChannelsStatusProjection.accept(current, newEpoch, "a", "a", 4, 3, true, false))
        assertSame(current, ChannelsStatusProjection.accept(current, newEpoch, "a", "a", 2, 3, true, false))
        assertSame(current, ChannelsStatusProjection.accept(current, newEpoch, "b", "a", 4, 3, true, false))
        assertSame(current, ChannelsStatusProjection.accept(current, newEpoch, null, "a", 4, 3, true, false))
        assertSame(current, ChannelsStatusProjection.accept(current, newEpoch, "a", "a", 4, 3, false, false))
        assertSame(current, ChannelsStatusProjection.accept(current, newEpoch, "a", "a", 4, 3, true, true))
    }

    @Test fun malformedTelemetryIsRejectedWithoutSessionEffect() {
        assertNotNull(ChannelsStatusProjection.parse(valid()))
        for (bad in listOf(valid().put("active", "12"), valid().put("active", 37), valid().put("target", 37),
            valid().put("active", 13).put("target", 12), valid().put("runtime_epoch", 0), valid().put("generation", -1),
            valid().put("type", "wake_status"), valid().put("extra", 1)))
            assertNull(ChannelsStatusProjection.parse(bad))
    }
}

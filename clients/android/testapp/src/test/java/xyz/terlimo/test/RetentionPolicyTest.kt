package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class RetentionPolicyTest {
    @Test fun defaultAndNonHoldModesDoNotKeepCpuAwake() {
        assertEquals(RetentionMode.BALANCED, RetentionMode.fromStored(null))
        assertEquals(RetentionMode.BALANCED, RetentionMode.fromStored("unknown"))
        for (mode in listOf(RetentionMode.BALANCED, RetentionMode.SAVE_BATTERY))
            assertEquals(RetentionLocks(false, false), RetentionPolicy.locks(mode, true, false, false, true, false, true))
    }
    @Test fun holdRetainsCpuAndOnlyScreenOffWifi() {
        assertEquals(RetentionLocks(true, true), state())
        assertEquals(RetentionLocks(true, false), state(interactive = true))
        assertEquals(RetentionLocks(true, false), state(wifi = false))
    }
    @Test fun stopPauseAndNetworkLossReleaseBothLocks() {
        assertEquals(RetentionLocks(false, false), state(running = false))
        assertEquals(RetentionLocks(false, false), state(paused = true))
        assertEquals(RetentionLocks(false, false), state(stopping = true))
        assertEquals(RetentionLocks(false, false), state(network = false))
    }
    private fun state(running: Boolean = true, paused: Boolean = false, stopping: Boolean = false,
        network: Boolean = true, interactive: Boolean = false, wifi: Boolean = true) =
        RetentionPolicy.locks(RetentionMode.HOLD_CONNECTION, running, paused, stopping, network, interactive, wifi)
}

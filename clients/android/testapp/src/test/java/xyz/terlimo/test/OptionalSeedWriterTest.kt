package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class OptionalSeedWriterTest {
    @Test fun foregroundIntentRejectsAlreadyQueuedOptionalWriteWithoutCancellingAttempt() {
        val gate = RecoveryCommitGate()
        gate.begin("attempt", "installation")
        val captured = gate.advanceForeground()
        var writes = 0
        gate.advanceForeground()
        assertFalse(gate.writeOptionalIfCurrent("attempt", "installation", captured) { writes++; true })
        assertEquals(0, writes)
        assertTrue(gate.writeIfCurrent("attempt", "installation") { writes++; true })
        assertEquals(1, writes)
    }
    @Test fun currentEpochWritesButCancelledOrOtherInstallationCannotWrite() {
        val gate = RecoveryCommitGate()
        gate.begin("a", "i")
        val epoch = gate.advanceForeground()
        assertTrue(gate.writeOptionalIfCurrent("a", "i", epoch) { true })
        assertFalse(gate.writeOptionalIfCurrent("a", "foreign", epoch) { error("foreign write") })
        gate.cancel()
        assertFalse(gate.writeOptionalIfCurrent("a", "i", epoch) { error("late write") })
    }
    @Test fun onlyCommandsAdvanceEpochNotProtocolRepliesOrStart() {
        for (reply in listOf("start", "sign_result", "persist_result", "vpn_result", "captcha_result"))
            assertFalse(RecoveryCommitGate.foregroundCommand(reply))
        for (command in listOf("device_wake", "refresh_manual", "explicit_connect", "plans_list",
            "payment_get", "devices_list", "usage_read", "cancel"))
            assertTrue(RecoveryCommitGate.foregroundCommand(command))
    }
}

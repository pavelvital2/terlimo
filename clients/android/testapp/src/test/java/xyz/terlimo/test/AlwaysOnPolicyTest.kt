package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class AlwaysOnPolicyTest {
    @Test fun markedAppEntryCannotConsumeOrBootstrapSystemGeneration() {
        val gate = AlwaysOnStartGate()
        assertEquals(AlwaysOnStartGate.Decision.IGNORE, gate.start(true, true, true, true))
        assertEquals(AlwaysOnStartGate.Decision.START, gate.start(false, true, true, true))
        repeat(3) { assertEquals(AlwaysOnStartGate.Decision.IGNORE, gate.start(false, true, true, true)) }
    }
    @Test fun lockedUnlockAndRevokeAreFencedWithoutStorageAccess() {
        val gate = AlwaysOnStartGate()
        var writes = 0
        assertEquals(AlwaysOnStartGate.Decision.WAIT_UNLOCK, gate.start(false, true, false, true))
        assertThrows(IllegalStateException::class.java) { withUnlockedStorage(false) { writes++ } }
        assertEquals(0, writes)
        gate.close()
        assertEquals(AlwaysOnStartGate.Decision.IGNORE, gate.start(false, true, true, true))
        val afterUnlock = AlwaysOnStartGate()
        assertEquals(AlwaysOnStartGate.Decision.WAIT_UNLOCK, afterUnlock.start(false, true, false, true))
        assertEquals(AlwaysOnStartGate.Decision.START, afterUnlock.start(false, true, true, true))
        repeat(3) { assertEquals(AlwaysOnStartGate.Decision.IGNORE, afterUnlock.start(false, true, true, true)) }
    }
    @Test fun lostConsentOrNonSystemNullEntryNeverStartsOrRetries() {
        for ((mode, consent) in listOf(false to true, true to false)) {
            val gate = AlwaysOnStartGate()
            assertEquals(AlwaysOnStartGate.Decision.REFUSE, gate.start(false, mode, true, consent))
            assertEquals(AlwaysOnStartGate.Decision.IGNORE, gate.start(false, true, true, true))
        }
    }
    @Test fun systemSelectionDoesNotIssueBusinessIntentOrUserUpdate() {
        val command = AutoConnectOriginPolicy.selectCommand("last-node", true)
        assertEquals(setOf("type", "node_id", "explicit_connect"), command.keys().asSequence().toSet())
        assertEquals("select_node", command.getString("type"))
        assertEquals("last-node", command.getString("node_id"))
        assertFalse(command.getBoolean("explicit_connect"))
        val updates = AppUpdateConnectionGate()
        updates.begin("system", AutoConnectOriginPolicy.userUpdate(true, false, false))
        assertFalse(updates.accepted("system"))
        updates.begin("user", AutoConnectOriginPolicy.userUpdate(false, false, false))
        assertTrue(updates.accepted("user")); assertFalse(updates.accepted("user"))
        assertTrue(AutoConnectOriginPolicy.selectCommand("last-node", false).getBoolean("explicit_connect"))
    }

    private fun snapshot(): AccountAccessSnapshot {
        val p = AccountAccessProjection(1, "2026-10-03T00:00:00Z", "1", null, "1",
            AccountAccessProjection.AccountAccessAccount("ACTIVE_PAID", true, "active", false, "acc-1"),
            AccountAccessProjection.AccountAccessEntitlement("paid", "active", 1, 1, "1", false, null, null, null),
            AccountAccessProjection.AccountAccessOnboarding("active", "server_confirmed_first_connection", null,
                "2026-10-03T00:10:00Z", 3600, true, false, false, false, false, "installation_fingerprint", "", ""),
            AccountAccessProjection.AccountAccessGrant(true, true, "subscription_data", "2026-10-03T00:10:00Z"))
        return AccountAccessSnapshot(p, 0, AccountAccessChain("1"))
    }
    @Test fun onlyCurrentUsableGrantPassesBeforeNativeAdmission() {
        val s = snapshot(); assertTrue(AlwaysOnAccess.usable(s, 0))
        assertFalse(AlwaysOnAccess.usable(s, 600_000))
        assertFalse(AlwaysOnAccess.usable(s.copy(current = false), 0))
        assertFalse(AlwaysOnAccess.usable(null, 0))
        for (state in listOf("EXPIRED", "REVOKED_SESSION", "VERIFIED_NO_SLOT"))
            assertFalse(AlwaysOnAccess.usable(s.copy(projection = s.projection.copy(account = s.projection.account.copy(state = state))), 0))
        for (binding in listOf("revoked", "deactivated"))
            assertFalse(AlwaysOnAccess.usable(s.copy(projection = s.projection.copy(account = s.projection.account.copy(bindingStatus = binding))), 0))
        for (status in listOf("expired", "revoked", "unknown_review"))
            assertFalse(AlwaysOnAccess.usable(s.copy(projection = s.projection.copy(entitlement = s.projection.entitlement.copy(status = status))), 0))
        val hour = s.copy(projection = s.projection.copy(grant = s.projection.grant.copy(dataAccess = "onboarding_hour")))
        assertTrue(AlwaysOnAccess.usable(hour, 0)); assertFalse(AlwaysOnAccess.usable(hour, 600_000))
        assertFalse(AlwaysOnAccess.usable(hour.copy(projection = hour.projection.copy(onboarding = hour.projection.onboarding.copy(state = "not_started"))), 0))
    }
}

package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Drives the real already-held KillSwitch branch decision and the stop-retention path:
 * a current `/me` snapshot must never survive a terminal stop/hold as "current", even
 * when the hold reason is unchanged, and no publish happens when nothing changes.
 */
class HoldRetentionTest {
    private fun projection() = AccountAccessProjection(
        accessVersion = 1,
        serverTime = "2026-09-25T10:00:00Z",
        sessionGeneration = "2",
        previousSessionGeneration = "1",
        accessRevision = "9",
        account = AccountAccessProjection.AccountAccessAccount(
            state = "ACTIVE_TRIAL", telegramLinked = true, bindingStatus = "active",
            managementOnly = false, accountRef = "acc-1",
        ),
        entitlement = AccountAccessProjection.AccountAccessEntitlement(
            type = "trial", status = "active", effectiveDeviceLimit = 3, slotsUsed = 5,
            revision = "9", perpetualCommercial = false,
            validFrom = null, validUntil = null, sourceRef = null,
        ),
        onboarding = AccountAccessProjection.AccountAccessOnboarding(
            state = "active", startedBy = "server_confirmed_first_connection",
            startedAt = "2026-09-25T09:30:00Z", notAfter = "2026-09-25T10:30:00Z",
            durationSeconds = 3600, oneTime = true, extendsOnRefresh = false,
            extendsOnRestart = false, createsTrial = false, requiresHardwareId = false,
            unit = "installation_fingerprint",
            postTelegramIdentity = "account_history_correlation",
            preTelegramReinstall = "may_be_indistinguishable_new_key_separate_unit",
        ),
        grant = AccountAccessProjection.AccountAccessGrant(
            controlAvailable = true, restrictedCheckoutAvailable = true,
            dataAccess = "subscription_data", effectiveDeadline = null,
        ),
        registration = AccountAccessProjection.Registration(
            "registered", true, true, "within_hour_no_prior_trial", true,
        ),
        trial = AccountAccessProjection.Trial("active", false, null, null, null),
    )

    private fun snapshot(current: Boolean) =
        AccountAccessSnapshot(projection(), 0L, AccountAccessChain(), current)

    @Test
    fun `same-code hold still invalidates a current snapshot`() {
        val next = HoldRetention.alreadyHeld(
            ViewState(phase = "KillSwitch", error = "SERVER_UNAVAILABLE", accountAccess = snapshot(true)),
            "SERVER_UNAVAILABLE",
        )
        assertNotNull(next)
        assertEquals("SERVER_UNAVAILABLE", next!!.error)
        assertFalse(next.accountAccess!!.current)
    }

    @Test
    fun `different-code hold updates the reason and invalidates`() {
        val next = HoldRetention.alreadyHeld(
            ViewState(phase = "KillSwitch", error = "OLD", accountAccess = snapshot(true)),
            "NEW",
        )
        assertNotNull(next)
        assertEquals("NEW", next!!.error)
        assertFalse(next.accountAccess!!.current)
    }

    @Test
    fun `unchanged reason and already-stale snapshot produce no publish`() {
        assertNull(HoldRetention.alreadyHeld(
            ViewState(phase = "KillSwitch", error = "SAME", accountAccess = snapshot(false)), "SAME"))
        assertNull(HoldRetention.alreadyHeld(ViewState(phase = "KillSwitch", error = "SAME"), "SAME"))
    }

    @Test
    fun `reason change without a snapshot still surfaces the new code`() {
        val next = HoldRetention.alreadyHeld(ViewState(phase = "KillSwitch", error = "OLD"), "NEW")
        assertNotNull(next)
        assertEquals("NEW", next!!.error)
        assertNull(next.accountAccess)
    }

    @Test
    fun `stop retention keeps the last-good projection but clears current`() {
        val stopped = SessionRetention.onStop(
            ViewState(phase = "CatalogReady", accountAccess = snapshot(true)), "Error", "TRANSPORT")
        assertEquals("Error", stopped.phase)
        assertEquals("TRANSPORT", stopped.error)
        assertTrue(stopped.accountAccess != null)
        assertFalse(stopped.accountAccess!!.current)
    }
}

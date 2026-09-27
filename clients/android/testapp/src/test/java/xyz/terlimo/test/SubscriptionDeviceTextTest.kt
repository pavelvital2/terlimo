package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class SubscriptionDeviceTextTest {
    private fun projection(
        type: String = "trial",
        status: String = "active",
        limit: Int = 2,
        used: Int = 1,
    ) = AccountAccessProjection(
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
            type = type, status = status, effectiveDeviceLimit = limit, slotsUsed = used,
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
        trial = AccountAccessProjection.Trial(
            state = "active", canActivate = false, reason = null,
            startsAt = "2026-09-25T09:00:00Z", endsAt = "2026-10-02T09:00:00Z",
        ),
    )

    private fun snapshot(p: AccountAccessProjection, current: Boolean = true) =
        AccountAccessSnapshot(p, 0L, AccountAccessChain(), current)

    @Test
    fun `current active entitlement renders usage from the entitlement fields`() {
        assertEquals("Устройства: 1 из 2", SubscriptionDeviceText.line(snapshot(projection(limit = 2, used = 1))))
        assertEquals("Устройства: 2 из 2", SubscriptionDeviceText.line(snapshot(projection(limit = 2, used = 2))))
        assertEquals("Устройства: 3 из 5", SubscriptionDeviceText.line(snapshot(projection(type = "paid", limit = 5, used = 3))))
    }

    @Test
    fun `absent account access shows nothing`() {
        assertNull(SubscriptionDeviceText.line(null))
    }

    @Test
    fun `retained last-good snapshot after a terminal stop is not shown as current`() {
        assertNull(SubscriptionDeviceText.line(snapshot(projection(limit = 3, used = 5), current = false)))
    }

    @Test
    fun `non-active entitlement shows nothing instead of a stale count`() {
        for (status in listOf("none", "expired", "revoked", "unknown_review")) {
            assertNull("status=$status", SubscriptionDeviceText.line(snapshot(projection(status = status))))
        }
    }

    @Test
    fun `active entitlement without a usable limit shows nothing`() {
        assertNull(SubscriptionDeviceText.line(snapshot(projection(status = "active", limit = 0, used = 0))))
    }

    @Test
    fun `server-inconsistent over-limit reports the exact reported numbers`() {
        assertEquals("Устройства: 5 из 3", SubscriptionDeviceText.line(snapshot(projection(limit = 3, used = 5))))
    }
}

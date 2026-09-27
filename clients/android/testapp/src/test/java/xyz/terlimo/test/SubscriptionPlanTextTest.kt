package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class SubscriptionPlanTextTest {
    private fun projection(
        type: String = "paid",
        status: String = "active",
        plan: AccountAccessProjection.AccountAccessPlan? = AccountAccessProjection.AccountAccessPlan("terlimo-30d", "30 дней", "days:30"),
    ) = AccountAccessProjection(
        accessVersion = 1, serverTime = "2026-09-25T10:00:00Z",
        sessionGeneration = "2", previousSessionGeneration = "1", accessRevision = "9",
        account = AccountAccessProjection.AccountAccessAccount(
            state = "ACTIVE_PAID", telegramLinked = true, bindingStatus = "active",
            managementOnly = false, accountRef = "acc-1"),
        entitlement = AccountAccessProjection.AccountAccessEntitlement(
            type = type, status = status, effectiveDeviceLimit = 2, slotsUsed = 1,
            revision = "9", perpetualCommercial = false,
            validFrom = null, validUntil = null, sourceRef = null, plan = plan),
        onboarding = AccountAccessProjection.AccountAccessOnboarding(
            state = "active", startedBy = "server_confirmed_first_connection",
            startedAt = "2026-09-25T09:30:00Z", notAfter = "2026-09-25T10:30:00Z", durationSeconds = 3600,
            oneTime = true, extendsOnRefresh = false, extendsOnRestart = false, createsTrial = false,
            requiresHardwareId = false, unit = "installation_fingerprint",
            postTelegramIdentity = "account_history_correlation",
            preTelegramReinstall = "may_be_indistinguishable_new_key_separate_unit"),
        grant = AccountAccessProjection.AccountAccessGrant(
            controlAvailable = true, restrictedCheckoutAvailable = true,
            dataAccess = "subscription_data", effectiveDeadline = null),
        registration = AccountAccessProjection.Registration("registered", true, true, "r", true),
        trial = AccountAccessProjection.Trial("none", false, null, null, null),
    )

    @Test
    fun `active paid and trial show the authoritative title`() {
        assertEquals("Тариф: 30 дней", SubscriptionPlanText.line(projection(type = "paid")))
        assertEquals("Тариф: 30 дней", SubscriptionPlanText.line(projection(type = "trial")))
    }

    @Test
    fun `absent null or blank title invents nothing`() {
        assertNull(SubscriptionPlanText.line(projection(plan = null)))
        assertNull(SubscriptionPlanText.line(projection(plan = AccountAccessProjection.AccountAccessPlan("x", null, "days:30"))))
        assertNull(SubscriptionPlanText.line(projection(plan = AccountAccessProjection.AccountAccessPlan("x", "   ", "days:30"))))
        assertNull(SubscriptionPlanText.line(null))
    }

    @Test
    fun `non-active entitlement is never labeled as an active plan`() {
        for (status in listOf("none", "expired", "revoked", "unknown_review")) {
            assertNull("status=$status", SubscriptionPlanText.line(projection(status = status)))
        }
    }
}

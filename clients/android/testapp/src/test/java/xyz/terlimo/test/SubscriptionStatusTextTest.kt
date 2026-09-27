package xyz.terlimo.test

import java.time.Instant
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

class SubscriptionStatusTextTest {
    private fun eventJson(
        deadline: String? = "2026-09-21T12:10:00Z",
        dataAccess: String = "subscription_data",
        onboardingState: String = "active",
        accountState: String = "ACTIVE_PAID",
        perpetualCommercial: Boolean = false,
        validUntil: String? = "2027-01-01T00:00:00Z",
    ): String {
        val deadlineValue = deadline?.let { "\"$it\"" } ?: "null"
        val validUntilValue = validUntil?.let { "\"$it\"" } ?: "null"
        val startedAt = if (onboardingState == "not_started") "null" else "\"2026-09-21T10:00:00Z\""
        val notAfter = if (onboardingState == "not_started") "null" else "\"2026-09-21T11:00:00Z\""
        return """
        {"v":1,"attempt_id":"attempt","type":"account_access","access_version":1,
         "server_time":"2026-09-21T12:00:00Z","session_generation":"1",
         "previous_session_generation":null,"access_revision":"7",
         "account":{"state":"$accountState","telegram_linked":true,"binding_status":"active",
            "management_only":false,"account_ref":"acc-1"},
         "entitlement":{"type":"paid","status":"active","valid_from":"2026-08-01T00:00:00Z",
            "valid_until":$validUntilValue,"effective_device_limit":5,"slots_used":2,
            "revision":"5","perpetual_commercial":$perpetualCommercial},
         "onboarding":{"state":"$onboardingState","started_by":"server_confirmed_first_connection",
            "started_at":$startedAt,"not_after":$notAfter,"duration_seconds":3600,"one_time":true,
            "extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
            "requires_hardware_id":false,"unit":"installation_fingerprint",
            "post_telegram_identity":"account_history_correlation",
            "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
         "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
            "data_access":"$dataAccess","effective_deadline":$deadlineValue}}
        """
    }

    private fun snapshot(json: String, receivedElapsed: Long = 50_000): AccountAccessSnapshot =
        AccountAccessPolicy.apply(null, AccountAccessParser.parse(JSONObject(json)), receivedElapsed)!!

    private fun summary() = CatalogSummary(
        subscriptionExpiresAt = Instant.parse("2027-01-01T00:00:00Z"),
        catalogExpiresAt = Instant.parse("2026-09-21T13:00:00Z"),
        issuedAt = Instant.parse("2026-09-21T12:00:00Z"),
        slotsUsed = 1,
        slotsLimit = 5,
    )

    @Test fun finiteSubscriptionShowsRemainingFromTheFrozenClock() {
        val accepted = snapshot(eventJson())
        assertEquals("Доступ активен · Подписка: осталось 10:00",
            SubscriptionStatusText.status(null, accepted, 50_000))
        assertTrue(SubscriptionStatusText.status(null, accepted, 150_000).endsWith("осталось 08:20"))
    }

    @Test fun perpetualSubscriptionHasNoCountdownLine() {
        val indefinite = JSONObject(eventJson(
            deadline = null, onboardingState = "not_started", perpetualCommercial = true))
            .apply { getJSONObject("entitlement").remove("valid_until") }.toString()
        val text = SubscriptionStatusText.status(null, snapshot(indefinite), 50_000)
        assertEquals("Доступ активен · Подписка активна", text)
        assertFalse(text.contains("осталось"))
    }

    @Test fun noAccessShowsTheVpnOffLine() {
        val text = SubscriptionStatusText.status(null,
            snapshot(eventJson(dataAccess = "restricted_checkout", deadline = null)), 0)
        assertTrue(text.contains("VPN не активен"))
        assertFalse(text.contains("осталось"))
    }

    @Test fun onboardingHourShowsTheOnboardingLine() {
        val accepted = snapshot(eventJson(dataAccess = "onboarding_hour", deadline = "2026-09-21T12:05:00Z"))
        assertEquals("Доступ для регистрации: осталось 05:00 · скоро завершится · Оформите доступ",
            SubscriptionStatusText.status(null, accepted, 0))
    }

    @Test fun expiredAccountShowsTheExpiredLine() {
        val text = SubscriptionStatusText.status(null, snapshot(eventJson(accountState = "EXPIRED")), 0)
        assertTrue(text.startsWith("Срок доступа истёк"))
    }

    @Test fun noSnapshotShowsNeutralPendingTextOnly() {
        val text = SubscriptionStatusText.status(null, null, 123_456)
        assertEquals(SubscriptionStatusText.PENDING, text)
        assertFalse(text.contains("осталось"))
        assertFalse(text.contains("Подписка"))
        assertFalse(text.contains("VPN"))
    }

    @Test fun malformedSnapshotIsRejectedAndNeverShown() {
        assertThrows(IllegalStateException::class.java) {
            AccountAccessParser.parse(JSONObject(eventJson(deadline = null, perpetualCommercial = false)))
        }
        assertEquals(SubscriptionStatusText.PENDING, SubscriptionStatusText.status(null, null, 0))
    }

    @Test fun catalogSummaryLineIsKeptAndNeverReplacesTheAccountStatus() {
        val pending = SubscriptionStatusText.status(summary(), null, 0)
        assertTrue(pending.contains("Места: 1 из 5"))
        assertTrue(pending.endsWith(SubscriptionStatusText.PENDING))

        val accepted = SubscriptionStatusText.status(summary(), snapshot(eventJson()), 50_000)
        assertTrue(accepted.contains("Места: 1 из 5"))
        assertTrue(accepted.endsWith("Доступ активен · Подписка: осталось 10:00"))
    }
}

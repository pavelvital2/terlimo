package xyz.terlimo.test

import java.time.ZoneId
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class SubscriptionTermTextTest {
    private val moscow: ZoneId = ZoneId.of("Europe/Moscow")

    private fun projection(
        entitlementType: String = "paid",
        entitlementStatus: String = "active",
        validUntil: String? = "2027-01-01T00:00:00Z",
        perpetual: Boolean = false,
        accountState: String = "ACTIVE_PAID",
        trial: String? = null,
    ): AccountAccessProjection {
        val validUntilValue = validUntil?.let { "\"$it\"" } ?: "null"
        val trialBlock = trial?.let { ",\"trial\":$it" } ?: ""
        val json = """
        {"v":1,"attempt_id":"attempt","type":"account_access","access_version":1,
         "server_time":"2026-09-24T10:00:00Z","session_generation":"2",
         "previous_session_generation":"1","access_revision":"9",
         "account":{"state":"$accountState","telegram_linked":true,"binding_status":"active",
            "management_only":false,"account_ref":"acc-1"},
         "entitlement":{"type":"$entitlementType","status":"$entitlementStatus",
            "valid_from":"2026-09-01T00:00:00Z","valid_until":$validUntilValue,
            "effective_device_limit":2,"slots_used":1,"revision":"9",
            "perpetual_commercial":$perpetual},
         "onboarding":{"state":"not_started","started_by":"server_confirmed_first_connection",
            "started_at":null,"not_after":null,"duration_seconds":3600,"one_time":true,
            "extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
            "requires_hardware_id":false,"unit":"installation_fingerprint",
            "post_telegram_identity":"account_history_correlation",
            "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
         "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
            "data_access":"subscription_data","effective_deadline":"2027-01-01T00:00:00Z"}$trialBlock}
        """.trimIndent()
        return AccountAccessParser.parse(JSONObject(json))
    }

    private fun perpetualProjection(): AccountAccessProjection {
        val event = JSONObject(
            """
            {"v":1,"attempt_id":"attempt","type":"account_access","access_version":1,
             "server_time":"2026-09-24T10:00:00Z","session_generation":"2",
             "previous_session_generation":"1","access_revision":"9",
             "account":{"state":"ACTIVE_PAID","telegram_linked":true,"binding_status":"active",
                "management_only":false,"account_ref":"acc-1"},
             "entitlement":{"type":"paid","status":"active","valid_from":"2026-09-01T00:00:00Z",
                "effective_device_limit":2,"slots_used":1,"revision":"9","perpetual_commercial":true},
             "onboarding":{"state":"not_started","started_by":"server_confirmed_first_connection",
                "started_at":null,"not_after":null,"duration_seconds":3600,"one_time":true,
                "extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
                "requires_hardware_id":false,"unit":"installation_fingerprint",
                "post_telegram_identity":"account_history_correlation",
                "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
             "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
                "data_access":"subscription_data","effective_deadline":null}}
            """.trimIndent())
        return AccountAccessParser.parse(event)
    }

    @Test
    fun paidExpiryIsRenderedInLocalDeviceTime() {
        assertEquals("Подписка действует до 01.01.2027 03:00 (местное время).",
            SubscriptionTermText.term(projection(), null, moscow))
        assertEquals("Подписка действует до 31.12.2026 22:00 (местное время).",
            SubscriptionTermText.term(projection(), null, ZoneId.of("Etc/GMT+2")))
    }

    @Test
    fun activeTrialUsesItsAcceptedEndInstant() {
        val trial = """{"state":"active","can_activate":false,"reason":null,
            "starts_at":"2026-09-24T10:00:00Z","ends_at":"2026-10-01T10:00:00Z"}"""
        assertEquals("Пробный доступ действует до 01.10.2026 13:00 (местное время).",
            SubscriptionTermText.term(projection(entitlementType = "trial", accountState = "ACTIVE_TRIAL", trial = trial),
                null, moscow))
    }

    @Test
    fun absentExpiryFieldIsNeverTurnedIntoAFabricatedDate() {
        val absent = projection(validUntil = null)
        val text = SubscriptionTermText.term(absent, null, moscow)
        assertEquals("Подписка активна, срок сервером не указан.", text)
        assertFalse(text!!.contains("до "))
        assertFalse(text.contains("2026"))
        assertFalse(text.contains("2027"))

        val trialActiveWithoutEnd = projection(
            entitlementType = "trial", accountState = "ACTIVE_TRIAL", validUntil = null).let {
            it.copy(trial = it.trial.copy(state = "active", endsAt = null))
        }
        assertEquals("Пробный доступ активен, срок сервером не указан.",
            SubscriptionTermText.term(trialActiveWithoutEnd, null, moscow))
    }

    @Test
    fun perpetualCommercialRightIsShownAsIndefiniteWithoutADate() {
        val text = SubscriptionTermText.term(perpetualProjection(), null, moscow)
        assertEquals("Подписка действует без ограничения срока.", text)
        assertFalse(text!!.contains("до "))
    }

    @Test
    fun expiredRevokedAndUnreviewedRightsAreTruthful() {
        val expired = projection(validUntil = "2026-09-01T00:00:00Z", accountState = "EXPIRED")
        assertEquals("Срок подписки истёк: 01.09.2026 03:00 (местное время).",
            SubscriptionTermText.term(expired, null, moscow))
        val expiredAccountOnly = projection(accountState = "EXPIRED", validUntil = null)
        assertEquals("Срок подписки истёк.", SubscriptionTermText.term(expiredAccountOnly, null, moscow))

        assertEquals("Подписка отозвана.",
            SubscriptionTermText.term(projection(entitlementStatus = "revoked"), null, moscow))
        assertEquals("Подписка на проверке.",
            SubscriptionTermText.term(projection(entitlementStatus = "unknown_review"), null, moscow))
        assertNull(SubscriptionTermText.term(projection(entitlementStatus = "none"), null, moscow))
    }

    @Test
    fun paidButFreshMeNotYetReflectingShowsPendingInsteadOfTheDate() {
        val awaiting = PurchaseFlow.paymentResult(PurchaseState(), PaymentStatusView(
            "pay-1", "paid", null, "9", "applied"))
        assertEquals(SubscriptionTermText.AWAITING_CONFIRMATION_TEXT,
            SubscriptionTermText.term(projection(), awaiting, moscow))
        assertFalse(SubscriptionTermText.term(projection(), awaiting, moscow)!!.contains("2027"))

        val confirmed = PurchaseFlow.onFreshMe(awaiting, projection())
        assertEquals("Подписка действует до 01.01.2027 03:00 (местное время).",
            SubscriptionTermText.term(projection(), confirmed, moscow))
        assertEquals(PurchaseFlow.CONFIRMED, confirmed.phase)
    }
}

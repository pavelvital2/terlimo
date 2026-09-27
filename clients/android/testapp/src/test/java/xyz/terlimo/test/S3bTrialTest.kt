package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

class S3bTrialTest {
    private fun eventJson(trial: String?): String {
        val block = trial?.let { ",\"trial\":$it" } ?: ""
        return """
        {"v":1,"attempt_id":"attempt","type":"account_access","access_version":1,
         "server_time":"2026-09-24T10:00:00Z","session_generation":"2",
         "previous_session_generation":"1","access_revision":"9",
         "account":{"state":"ACTIVE_TRIAL","telegram_linked":true,"binding_status":"active",
            "management_only":false,"account_ref":"acc-1"},
         "entitlement":{"type":"trial","status":"active","valid_from":"2026-09-24T10:00:00Z",
            "valid_until":"2026-10-01T10:00:00Z","effective_device_limit":2,"slots_used":1,
            "revision":"9","perpetual_commercial":false},
         "onboarding":{"state":"active","started_by":"server_confirmed_first_connection",
            "started_at":"2026-09-24T09:30:00Z","not_after":"2026-09-24T10:30:00Z","duration_seconds":3600,
            "one_time":true,"extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
            "requires_hardware_id":false,"unit":"installation_fingerprint",
            "post_telegram_identity":"account_history_correlation",
            "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
         "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
            "data_access":"subscription_data","effective_deadline":"2026-10-01T10:00:00Z"},
         "registration":{"state":"registered","within_hour":true,"trial_available":true,
            "trial_reason":"within_hour_no_prior_trial","purchase_available":true}$block}
        """
    }

    private fun projection(trial: AccountAccessProjection.Trial) = AccountAccessProjection(
        accessVersion = 1, serverTime = "2026-09-24T10:00:00Z", sessionGeneration = "2",
        previousSessionGeneration = "1", accessRevision = "9",
        account = AccountAccessProjection.AccountAccessAccount(
            state = "ACTIVE_TRIAL", telegramLinked = true, bindingStatus = "active",
            managementOnly = false, accountRef = "acc-1"),
        entitlement = AccountAccessProjection.AccountAccessEntitlement(
            type = "trial", status = "active", effectiveDeviceLimit = 2, slotsUsed = 1,
            revision = "9", perpetualCommercial = false, validFrom = null, validUntil = null, sourceRef = null),
        onboarding = AccountAccessProjection.AccountAccessOnboarding(
            state = "active", startedBy = "server_confirmed_first_connection",
            startedAt = "2026-09-24T09:30:00Z", notAfter = "2026-09-24T10:30:00Z", durationSeconds = 3600,
            oneTime = true, extendsOnRefresh = false, extendsOnRestart = false, createsTrial = false,
            requiresHardwareId = false, unit = "installation_fingerprint",
            postTelegramIdentity = "account_history_correlation",
            preTelegramReinstall = "may_be_indistinguishable_new_key_separate_unit"),
        grant = AccountAccessProjection.AccountAccessGrant(
            controlAvailable = true, restrictedCheckoutAvailable = true,
            dataAccess = "subscription_data", effectiveDeadline = null),
        registration = AccountAccessProjection.Registration("registered", true, true, "within_hour_no_prior_trial", true),
        trial = trial,
    )

    private fun state(trial: AccountAccessProjection.Trial, display: TrialState? = null) = ViewState(
        accountAccess = AccountAccessSnapshot(projection(trial), 0L, AccountAccessChain()),
        trial = display,
    )

    @Test
    fun `trial block parses and absent trial means no endpoint and no action`() {
        val parsed = AccountAccessParser.parse(JSONObject(eventJson(
            """{"state":"available","can_activate":true,"reason":null,"starts_at":null,"ends_at":null}""")))
        assertEquals("available", parsed.trial.state)
        assertTrue(parsed.trial.canActivate)

        val absent = AccountAccessParser.parse(JSONObject(eventJson(null)))
        assertEquals("none", absent.trial.state)
        assertFalse(absent.trial.canActivate)
        assertFalse(TrialUi.activateVisible(state(absent.trial)))
        assertNull(TrialUi.statusText(state(absent.trial)))
    }

    @Test
    fun `trial block rejects unknown state and active without instants`() {
        assertThrows(IllegalStateException::class.java) {
            AccountAccessParser.parse(JSONObject(eventJson(
                """{"state":"weird","can_activate":false,"reason":null,"starts_at":null,"ends_at":null}""")))
        }
        assertThrows(IllegalStateException::class.java) {
            AccountAccessParser.parse(JSONObject(eventJson(
                """{"state":"active","can_activate":false,"reason":null,"starts_at":null,"ends_at":null}""")))
        }
    }

    @Test
    fun `activate action follows the server can_activate signal only`() {
        assertTrue(TrialUi.activateVisible(state(
            AccountAccessProjection.Trial("available", true, null, null, null))))
        assertFalse(TrialUi.activateVisible(state(
            AccountAccessProjection.Trial("used", false, "trial_already_used", null, null))))
        assertFalse(TrialUi.activateVisible(state(
            AccountAccessProjection.Trial("ineligible", false, "hour_expired_before_registration", null, null))))
        // A fixed error suppresses the action until a fresh server state arrives.
        assertFalse(TrialUi.activateVisible(state(
            AccountAccessProjection.Trial("available", true, null, null, null),
            TrialState(state = "available", canActivate = true, error = "CHANNEL_MEMBERSHIP_REQUIRED"))))
    }

    @Test
    fun `status text covers channel-required used expired and available states`() {
        fun errorState(code: String) = state(
            AccountAccessProjection.Trial("available", false, null, null, null), TrialState(error = code))
        assertEquals("Нужна подписка на официальный канал TERLIMO. Подпишитесь и повторите проверку.",
            TrialUi.statusText(errorState("CHANNEL_MEMBERSHIP_REQUIRED")))
        assertEquals("Пробный доступ уже использован.", TrialUi.statusText(errorState("TRIAL_ALREADY_USED")))
        assertEquals("Сначала зарегистрируйтесь в Telegram.", TrialUi.statusText(errorState("REGISTRATION_REQUIRED")))
        assertEquals("У вас уже действует подписка.", TrialUi.statusText(errorState("SUBSCRIPTION_ACTIVE")))

        assertEquals("Доступен пробный доступ на 7 дней.", TrialUi.statusText(state(
            AccountAccessProjection.Trial("available", true, null, null, null))))
        // A used/expired replay is never shown as active and offers no re-issuance.
        assertEquals("Пробный доступ уже использован.", TrialUi.statusText(state(
            AccountAccessProjection.Trial("used", false, "trial_already_used", null, null))))
        assertEquals("Пробный доступ недоступен: час истёк до регистрации.", TrialUi.statusText(state(
            AccountAccessProjection.Trial("ineligible", false, "hour_expired_before_registration", null, null))))
        assertEquals("Пробный доступ активен.", TrialUi.statusText(state(
            AccountAccessProjection.Trial("active", false, null, "2026-09-24T10:00:00Z", "2026-10-01T10:00:00Z"))))
    }
}

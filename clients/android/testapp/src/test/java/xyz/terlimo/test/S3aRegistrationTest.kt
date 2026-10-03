package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

class S3aRegistrationTest {
    private fun eventJson(registration: String?): String {
        val block = registration?.let { ",\"registration\":$it" } ?: ""
        return """
        {"v":1,"attempt_id":"attempt","type":"account_access","access_version":1,
         "server_time":"2026-09-24T10:00:00Z","session_generation":"1",
         "previous_session_generation":null,"access_revision":"4",
         "account":{"state":"UNLINKED","telegram_linked":false,"binding_status":"none",
            "management_only":false,"account_ref":null},
         "entitlement":{"type":"none","status":"none","valid_from":null,"valid_until":null,
            "effective_device_limit":1,"slots_used":0,"revision":"1","perpetual_commercial":false},
         "onboarding":{"state":"active","started_by":"server_confirmed_first_connection",
            "started_at":"2026-09-24T09:30:00Z","not_after":"2026-09-24T10:30:00Z","duration_seconds":3600,
            "one_time":true,"extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
            "requires_hardware_id":false,"unit":"installation_fingerprint",
            "post_telegram_identity":"account_history_correlation",
            "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
         "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
            "data_access":"onboarding_hour","effective_deadline":"2026-09-24T10:30:00Z"}$block}
        """
    }

    private fun parse(registration: String?) =
        AccountAccessParser.parse(JSONObject(eventJson(registration)))

    private fun projection(
        dataAccess: String = "onboarding_hour",
        registration: AccountAccessProjection.Registration =
            AccountAccessProjection.Registration("none", false, false, null, true),
    ) = AccountAccessProjection(
        accessVersion = 1, serverTime = "2026-09-24T10:00:00Z", sessionGeneration = "1",
        previousSessionGeneration = null, accessRevision = "4",
        account = AccountAccessProjection.AccountAccessAccount(
            state = if (registration.state == "registered") "VERIFIED_NO_ENTITLEMENT" else "UNLINKED",
            telegramLinked = registration.state == "registered",
            bindingStatus = if (registration.state == "registered") "active" else "none",
            managementOnly = false, accountRef = if (registration.state == "registered") "account-A" else null),
        entitlement = AccountAccessProjection.AccountAccessEntitlement(
            type = "none", status = "none", effectiveDeviceLimit = 1, slotsUsed = 0,
            revision = "1", perpetualCommercial = false, validFrom = null, validUntil = null, sourceRef = null),
        onboarding = AccountAccessProjection.AccountAccessOnboarding(
            state = "active", startedBy = "server_confirmed_first_connection",
            startedAt = "2026-09-24T09:30:00Z", notAfter = "2026-09-24T10:30:00Z", durationSeconds = 3600,
            oneTime = true, extendsOnRefresh = false, extendsOnRestart = false, createsTrial = false,
            requiresHardwareId = false, unit = "installation_fingerprint",
            postTelegramIdentity = "account_history_correlation",
            preTelegramReinstall = "may_be_indistinguishable_new_key_separate_unit"),
        grant = AccountAccessProjection.AccountAccessGrant(
            controlAvailable = true, restrictedCheckoutAvailable = true,
            dataAccess = dataAccess, effectiveDeadline = "2026-09-24T10:30:00Z"),
        registration = registration,
    )

    private fun state(projection: AccountAccessProjection?) = ViewState(
        accountAccess = projection?.let { AccountAccessSnapshot(it, 0L, AccountAccessChain()) })

    @Test
    fun `registration block is parsed and absent block defaults to none`() {
        val parsed = parse("""{"state":"registered","within_hour":true,"trial_available":false,
            "trial_reason":"hour_expired","purchase_available":true}""")
        assertEquals("registered", parsed.registration.state)
        assertTrue(parsed.registration.withinHour)
        assertFalse(parsed.registration.trialAvailable)
        assertEquals("hour_expired", parsed.registration.trialReason)
        assertTrue(parsed.registration.purchaseAvailable)

        val absent = parse(null)
        assertEquals("none", absent.registration.state)
        assertNull(absent.registration.trialReason)
        assertFalse(absent.registration.purchaseAvailable)
    }

    @Test
    fun `registration block rejects unknown state and reason`() {
        assertThrows(IllegalStateException::class.java) {
            parse("""{"state":"weird","within_hour":false,"trial_available":false,
                "trial_reason":null,"purchase_available":true}""")
        }
        assertThrows(IllegalStateException::class.java) {
            parse("""{"state":"registered","within_hour":false,"trial_available":false,
                "trial_reason":"made_up","purchase_available":true}""")
        }
    }

    @Test
    fun `registration action is offered without an hour and not once registered`() {
        assertTrue(RegistrationUi.registerVisible(state(projection())))
        assertFalse(RegistrationUi.registerVisible(
            state(projection(registration = AccountAccessProjection.Registration("registered", true, true, null, true)))))
        assertTrue(RegistrationUi.registerVisible(
            state(projection(dataAccess = "subscription_data"))))
        assertFalse(RegistrationUi.registerVisible(state(null)))
    }

    @Test
    fun `status text explains registration independently of trial eligibility`() {
        val pending = state(projection()).copy(registration = RegistrationState(state = "pending"))
        assertEquals("Завершите регистрацию в Telegram.", RegistrationUi.statusText(pending))
        assertEquals("Ожидаем подтверждение в Telegram", RegistrationUi.buttonText(pending))

        val expired = state(projection(registration = AccountAccessProjection.Registration("registered", false, false, "hour_expired", true))).copy(registration = RegistrationState(state = "registered",
            withinHour = false, trialAvailable = false, trialReason = "hour_expired", purchaseAvailable = true))
        assertEquals("Вход через Telegram подтверждён.",
            RegistrationUi.statusText(expired))

        val used = state(projection(registration = AccountAccessProjection.Registration("registered", false, false, "trial_already_used", true))).copy(registration = RegistrationState(state = "registered",
            withinHour = false, trialAvailable = false, trialReason = "trial_already_used", purchaseAvailable = true))
        assertEquals("Вход через Telegram подтверждён.", RegistrationUi.statusText(used))

        val available = state(projection(registration = AccountAccessProjection.Registration("registered", true, true, "within_hour_no_prior_trial", true))).copy(registration = RegistrationState(state = "registered",
            withinHour = true, trialAvailable = true, trialReason = "within_hour_no_prior_trial", purchaseAvailable = true))
        assertEquals("Вход через Telegram подтверждён.",
            RegistrationUi.statusText(available))

        val disabled = state(projection()).copy(registration = RegistrationState(state = "none", error = "REGISTRATION_DISABLED"))
        assertEquals("Регистрация временно недоступна.", RegistrationUi.statusText(disabled))
        assertEquals("Повторить регистрацию в Telegram", RegistrationUi.buttonText(disabled))
    }

    @Test
    fun coldRegistrationRechecksServerEligibilityAfterMe() {
        val hour = state(projection())
        val expired = state(projection(dataAccess = "none"))
        val registered = state(projection(registration = AccountAccessProjection.Registration(
            "registered", false, false, null, true)))
        val paidExpired = expired.copy(purchase = PurchaseFlow.paymentResult(null, paidPayment()))
        for ((fresh, expected) in listOf(hour to true, expired to true, paidExpired to true, registered to false)) {
            val gate = RegistrationLoginGate()
            assertEquals(RegistrationLoginTapAction.START_SERVICE,
                gate.onTap(null, RegistrationAction.REGISTER, RegistrationAction.REGISTER.eligible(hour)))
            gate.onAttemptStarted("cold")
            val decision = gate.onVerifiedRights("cold") { it.eligible(fresh) }
            assertEquals(if (expected) RegistrationVerified.Send(RegistrationAction.REGISTER)
                else RegistrationVerified.Refused, decision)
            assertTrue(gate.onResolved("cold"))
        }
        assertTrue(RegistrationAction.REFRESH.eligible(hour.copy(registration = RegistrationState(state = "pending"))))
        assertFalse(RegistrationAction.REFRESH.eligible(registered.copy(registration = RegistrationState(state = "registered"))))
    }

    private fun paidPayment() = PaymentStatusView(
        paymentId = "pay-1", paymentStatus = "paid", checkoutReference = "https://pay.example/s/1",
        creditedEntitlementRevision = null, accessApplicationState = "pending")

    @Test
    fun `a parked paid order offers mandatory registration after the hour expired`() {
        val expired = projection(dataAccess = "none",
            registration = AccountAccessProjection.Registration("none", false, false, null, true))
        val base = state(expired)
        assertTrue(RegistrationUi.registerVisible(base))
        val paid = base.copy(purchase = PurchaseFlow.paymentResult(
            PurchaseFlow.plansLoaded(null, "5", emptyList()), paidPayment()))
        assertTrue(PurchaseFlow.paidAwaitingBinding(paid.purchase))
        assertTrue(RegistrationUi.registerVisible(paid))
        assertEquals("Оплата получена. Зарегистрируйтесь в Telegram, чтобы применить оплаченный доступ.",
            RegistrationUi.statusText(paid))
        assertEquals("Оплата получена. Зарегистрируйтесь в Telegram, чтобы применить доступ.",
            PaymentsText.purchaseStatus(paid.purchase, expired, java.time.ZoneId.of("UTC")))
        // A registration link alone does not upgrade the old unlinked bearer.
        val registered = paid.copy(accountAccess = AccountAccessSnapshot(
            expired.copy(registration = AccountAccessProjection.Registration("registered", false, false, null, true)),
            0L, AccountAccessChain()))
        assertTrue(RegistrationUi.registerVisible(registered))
        // Registration no longer depends on payment state, even after a historical confirmation.
        val confirmed = paid.copy(purchase = paid.purchase!!.copy(phase = PurchaseFlow.CONFIRMED))
        assertFalse(PurchaseFlow.paidAwaitingBinding(confirmed.purchase))
        assertTrue(RegistrationUi.registerVisible(confirmed))
    }

    @Test
    fun `an explicit registration error stays visible above the paid call`() {
        val expired = projection(dataAccess = "none",
            registration = AccountAccessProjection.Registration("none", false, false, null, true))
        val paid = state(expired).copy(
            purchase = PurchaseFlow.paymentResult(
                PurchaseFlow.plansLoaded(null, "5", emptyList()), paidPayment()),
            registration = RegistrationState(error = "ACCESS_DENIED"))
        assertEquals("Регистрация недоступна для этой установки.", RegistrationUi.statusText(paid))
        assertTrue(RegistrationUi.registerVisible(paid))
    }
}

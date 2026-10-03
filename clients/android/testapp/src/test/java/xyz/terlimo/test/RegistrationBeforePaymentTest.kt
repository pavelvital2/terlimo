package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import java.time.ZoneOffset

class RegistrationBeforePaymentTest {
    private fun snapshot(registered: Boolean = false, available: Boolean = true): AccountAccessSnapshot {
        val projection = AccountAccessParser.parse(JSONObject("""
            {"v":1,"attempt_id":"fresh","type":"account_access","access_version":1,
             "server_time":"2026-10-03T06:00:00Z","session_generation":"1",
             "previous_session_generation":null,"access_revision":"4",
             "account":{"state":"${if (registered) "ACTIVE_PAID" else "UNLINKED"}",
                "telegram_linked":$registered,"binding_status":"${if (registered) "active" else "none"}",
                "management_only":false,"account_ref":${if (registered) "\"account-A\"" else "null"}},
             "entitlement":{"type":"none","status":"none","valid_from":null,"valid_until":null,
                "effective_device_limit":1,"slots_used":0,"revision":"1","perpetual_commercial":false},
             "onboarding":{"state":"expired","started_by":"server_confirmed_first_connection",
                "started_at":"2026-10-02T01:00:00Z","not_after":"2026-10-02T02:00:00Z","duration_seconds":3600,
                "one_time":true,"extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
                "requires_hardware_id":false,"unit":"installation_fingerprint",
                "post_telegram_identity":"account_history_correlation",
                "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
             "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
                "data_access":"none","effective_deadline":null},
             "registration":{"state":"${if (registered) "registered" else "none"}","within_hour":false,
                "trial_available":false,"trial_reason":"hour_expired","purchase_available":$available}}
        """.trimIndent()))
        return AccountAccessSnapshot(projection, 1L, AccountAccessChain())
    }
    private val newOperations = listOf(PurchaseOperation.Plans,
        PurchaseOperation.Quote("p30", "days:30", "sbp"), PurchaseOperation.Payment("q1"))

    @Test fun expiredUnregisteredHasRegistrationAndLoginButNoPurchaseOrTrial() {
        val me = snapshot()
        val state = ViewState(accountAccess = me, registration = RegistrationState())
        assertTrue(RegistrationUi.registerVisible(state))
        assertTrue(RegistrationUi.loginVisible(state))
        assertFalse(PurchaseFlow.offered(me.projection))
        assertFalse(TrialUi.activateVisible(state))
        assertEquals(RegistrationUi.INTERNET_TEXT, RegistrationUi.statusText(state))
        assertEquals(PaymentsText.REGISTRATION_FIRST_TEXT,
            PaymentsText.purchaseStatus(PurchaseState(), me.projection, ZoneOffset.UTC))
        for (op in newOperations) assertFalse(PurchaseFlow.canPrepare(op, me, null))
        assertEquals("expired", me.projection.onboarding.state)
    }

    @Test fun onlyFreshRegisteredBoundAccountAndServerAvailabilityPermitPrepare() {
        val good = snapshot(true)
        assertTrue(PurchaseFlow.offered(good.projection))
        for (op in newOperations) {
            assertTrue(PurchaseFlow.canPrepare(op, good, "account-A"))
            assertFalse(PurchaseFlow.canPrepare(op, snapshot(true, false), "account-A"))
            assertFalse(PurchaseFlow.canPrepare(op, good.copy(current = false), "account-A"))
            assertFalse(PurchaseFlow.canPrepare(op, good, null))
            assertFalse(PurchaseFlow.canPrepare(op, good, "account-B"))
            assertFalse(PurchaseFlow.canPrepare(op, good.copy(projection = good.projection.copy(
                account = good.projection.account.copy(bindingStatus = "pending"))), "account-A"))
            assertFalse(PurchaseFlow.canPrepare(op, good.copy(projection = good.projection.copy(
                account = good.projection.account.copy(accountRef = null))), null))
            assertFalse(PurchaseFlow.canPrepare(op, good.copy(projection = good.projection.copy(
                account = good.projection.account.copy(bindingStatus = "revoked"))), "account-A"))
        }
        // A registration-status UI event is not a fresh authenticated /me.
        val pending = ViewState(accountAccess = snapshot(), registration = RegistrationState(state = "registered"))
        assertFalse(PurchaseFlow.offered(pending.accountAccess!!.projection))
        assertFalse(TrialUi.activateVisible(ViewState(accountAccess = good)))
    }

    @Test fun managedWithoutRegistrationLinkCanBuyAndDoesNotNeedLogin() {
        val base = snapshot(true)
        val managed = base.copy(projection = base.projection.copy(
            registration = base.projection.registration.copy(state = "none")))
        val state = ViewState(accountAccess = managed, registration = RegistrationState(state = "none"))
        assertTrue(PurchaseFlow.offered(managed.projection))
        assertFalse(RegistrationUi.registerVisible(state))
        assertFalse(RegistrationUi.loginVisible(state))
        assertTrue(RegistrationUi.statusText(state)!!.startsWith("Вход через Telegram подтверждён."))
        assertEquals(PaymentsText.CHECK_AVAILABILITY_TEXT,
            PaymentsText.purchaseStatus(PurchaseState(), managed.projection, ZoneOffset.UTC))
        for (op in newOperations) assertTrue(PurchaseFlow.canPrepare(op, managed, "account-A"))
        // Display may retain accepted rights; actual sending must reacquire fresh /me.
        assertFalse(PurchaseFlow.canPrepare(newOperations.last(), managed.copy(current = false), "account-A"))
        assertFalse(PurchaseFlow.canPrepare(newOperations.last(), managed, "account-B"))
        val noSlot = managed.copy(projection = managed.projection.copy(
            account = managed.projection.account.copy(bindingStatus = "none")))
        val unavailable = managed.copy(projection = managed.projection.copy(
            registration = managed.projection.registration.copy(purchaseAvailable = false)))
        for (blocked in listOf(noSlot, unavailable)) {
            assertFalse(PurchaseFlow.offered(blocked.projection))
            for (op in newOperations) assertFalse(PurchaseFlow.canPrepare(op, blocked, "account-A"))
        }
        // Provider availability is not a reason to register a usable account again.
        assertFalse(RegistrationUi.registerVisible(state.copy(accountAccess = unavailable)))
    }

    @Test fun registeredOldUnlinkedBearerAndRawEventsDoNotProveEligibility() {
        val old = snapshot(available = false)
        val registered = old.copy(projection = old.projection.copy(
            registration = old.projection.registration.copy(state = "registered")))
        val view = ViewState(accountAccess = registered,
            registration = RegistrationState(state = "registered", purchaseAvailable = true))
        assertFalse(PurchaseFlow.offered(registered.projection))
        assertTrue(RegistrationUi.registerVisible(view))
        assertTrue(RegistrationUi.loginVisible(view))
        assertEquals(PaymentsText.REGISTRATION_FIRST_TEXT,
            PaymentsText.purchaseStatus(PurchaseState(), registered.projection, ZoneOffset.UTC))
        for (op in newOperations) assertFalse(PurchaseFlow.canPrepare(op, registered, "account-A"))
        // Even a contradictory availability flag cannot bypass account/binding proof.
        assertFalse(PurchaseFlow.offered(registered.projection.copy(
            registration = registered.projection.registration.copy(purchaseAvailable = true))))
    }

    @Test fun queuedPayRechecksChangedBindingBeforeAnyIntentKeysOrWrite() {
        var atPrepare = snapshot(true)
        assertTrue(PurchaseFlow.canPrepare(PurchaseOperation.Payment("q1"), atPrepare, "account-A"))
        atPrepare = snapshot() // registration lost after the UI queued Pay
        val single = PurchaseSingleFlight()
        var intentWrites = 0
        var nativeWrites = 0
        val result = PurchaseSender(single).send(PurchaseFlight(PurchaseFlightKind.PAYMENT, "fresh", "q1"), {
            check(PurchaseFlow.canPrepare(PurchaseOperation.Payment("q1"), atPrepare, "account-A"))
            intentWrites++
            JSONObject()
        }, { nativeWrites++; true })
        assertEquals(PurchaseSendOutcome.LOCAL_FAILURE, result)
        assertEquals(0, intentWrites)
        assertEquals(0, nativeWrites)
        assertFalse(single.busy())
        assertEquals(PaymentsText.REGISTRATION_FIRST_TEXT, PaymentsText.errorText("TELEGRAM_REQUIRED"))
    }

    @Test fun oldOrderRecoveryAndErrorDisplayAreNotReclassifiedAsNewPurchase() {
        val me = snapshot()
        for (op in listOf(PurchaseOperation.Recover, PurchaseOperation.PaymentGet("existing"),
            PurchaseOperation.Payment("original-quote", recovery = true))) {
            // Eligibility passes through to the unchanged account-bound durable guard.
            assertTrue(PurchaseFlow.canPrepare(op, me, null))
        }
        val legacy = PurchaseState(recovery = "legacy_unknown", phase = PurchaseFlow.ERROR)
        assertTrue(PaymentsText.purchaseStatus(legacy, me.projection, ZoneOffset.UTC).contains("поддержки"))
        val error = PurchaseFlow.failure(PurchaseState(), "TRANSPORT")
        assertEquals(PaymentsText.errorText("TRANSPORT"),
            PaymentsText.purchaseStatus(error, me.projection, ZoneOffset.UTC))
    }
}

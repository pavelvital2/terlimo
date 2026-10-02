package xyz.terlimo.test

import java.time.Instant
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

/** Restart tests use encoded disk bytes and fresh policy instances, with no provider/device. */
class PurchaseRecoveryTest {
    private class Disk : PurchaseAttemptStore {
        var bytes: String? = null
        var writes = 0
        var fail = false
        override fun read() = bytes
        override fun write(encoded: String) {
            if (fail) error("disk full")
            bytes = encoded
            writes++
        }
    }
    private val owner = "account-recovery-test"
    private fun event(type: String, scenario: String = "selected_renewal"): JSONObject {
        val rows = JSONArray(javaClass.getResource("/payment-reviewed-bridge.json")!!.readText())
        return (0 until rows.length()).map { rows.getJSONObject(it) }
            .first { it.getString("scenario") == scenario && it.getJSONObject("event").getString("type") == type }
            .getJSONObject("event")
    }
    private fun quoteEvent() = event(PaymentsContract.TYPE_QUOTE_CREATE_RESULT)
    private fun quote() = (PaymentsContract.parse(quoteEvent()) as PaymentsEvent.Quote).quote
    private fun plans() = (PaymentsContract.parse(event(PaymentsContract.TYPE_PLANS_LIST_RESULT)) as PaymentsEvent.Plans).plans
    private fun selection(): PurchaseSelection {
        val q = quote()
        return PurchaseSelection.fromPlan(plans().first { it.planId == q.product!!.planId }, q.method,
            q.product!!.renewExtraSlotIds)!!
    }
    private fun prepared(disk: Disk): PurchaseAttempts = PurchaseAttempts(disk).also {
        it.beginQuote(selection())
        it.bindQuote(quote().quoteId, quoteEvent().toString())
    }
    private fun paidEvent() = event(PaymentsContract.TYPE_PAYMENT_GET_RESULT)
    private fun assertRejected(block: () -> Unit) {
        try { block(); fail("must refuse without changing the first order") } catch (_: IllegalStateException) { }
    }

    @Test fun unknownWriteRestartsWithFrozenSelectionAndSameKeysEvenAfterExpiry() {
        val disk = Disk()
        val first = prepared(disk)
        val flight = PurchaseSingleFlight()
        val send = PurchaseSender(flight).send(PurchaseFlight(PurchaseFlightKind.PAYMENT, "native-1"), {
            val record = first.markCreate(owner, quote())
            JSONObject().put("quote_id", record.quoteId).put("idempotency_key", record.paymentKey)
        }, {
            assertTrue(PurchaseAttemptCodec.decode(disk.bytes)!!.unresolved) // before native write
            false // server may have accepted it; response/write completion lost
        })
        assertEquals(PurchaseSendOutcome.UNKNOWN_WRITE, send)
        val saved = first.current()!!
        val writes = disk.writes
        val restarted = PurchaseAttempts(disk) { error("recovery must never generate a new key") }
        val op = restarted.recoveryOperation(owner) as PurchaseOperation.Payment
        assertTrue(op.recovery)
        assertEquals(saved.quoteId, op.quoteId)
        assertEquals(saved, restarted.retryCreate(owner))
        assertEquals(saved.quoteKey, restarted.current()!!.quoteKey)
        assertEquals(saved.paymentKey, restarted.current()!!.paymentKey)
        assertEquals(selection(), restarted.current()!!.selection)
        val restored = restarted.recoveryState(owner)!!
        assertEquals(quote(), restored.quote)
        assertTrue(PurchaseFlow.quoteExpired(restored, Instant.parse("2027-01-01T00:00:00Z")))
        assertNull(restored.quoteBinding)
        assertNull(restored.createAck)
        assertTrue(PurchaseFlow.blocksNewPurchase(restored))
        assertRejected { restarted.beginQuote(selection().copy(method = "card")) }
        assertFalse(restarted.restart())
        assertEquals(writes, disk.writes)
        val refreshed = PurchaseFlow.plansLoaded(restored, "new", emptyList())
        assertEquals(restored.quote, refreshed.quote)
        assertEquals(restored.selectedRenewExtraSlotIds, refreshed.selectedRenewExtraSlotIds)
        val failedRefresh = PurchaseFlow.plansFailure(refreshed, "TRANSPORT")
        assertEquals(restored.quote, failedRefresh.quote)
        assertEquals(restored.selectedRenewExtraSlotIds, failedRefresh.selectedRenewExtraSlotIds)
    }

    @Test fun knownPaymentRestartsOnExistingStatusWithoutCreateOrBrowserAck() {
        val disk = Disk()
        val first = prepared(disk)
        first.markCreate(owner, quote())
        val pending = paidEvent().put("payment_status", "pending").put("credit_state", "unapplied")
            .put("credited_product", JSONObject.NULL).put("credited_entitlement_revision", JSONObject.NULL)
        val saved = first.savePayment(owner, pending.toString())
        val restarted = PurchaseAttempts(disk)
        assertEquals(PurchaseOperation.PaymentGet(saved.order!!.payment!!.paymentId), restarted.recoveryOperation(owner))
        assertRejected { restarted.retryCreate(owner) }
        val state = restarted.recoveryState(owner)!!
        assertEquals(saved.order.payment, state.payment)
        assertNull(state.createAck)
        val browser = CheckoutOpenPolicy()
        browser.restore(null)
        assertNull(browser.autoOpenAfterPay(state.createAck))
        assertNotNull(browser.onContinueRequested(state.payment)) // only explicit continuation
        assertFalse(state.freshMeConfirmed)
        assertTrue(PurchaseFlow.blocksNewPurchase(state))
        assertFalse(restarted.restart())
    }

    @Test fun paidReviewSurvivesRestartAndCannotBeReleasedByBrowserOrExistingRights() {
        val disk = Disk()
        val first = prepared(disk)
        first.markCreate(owner, quote())
        val review = paidEvent().put("credit_state", "needs_review").put("credit_review_reason", "target_expired")
            .put("credited_product", JSONObject.NULL).put("credited_entitlement_revision", JSONObject.NULL)
        first.savePayment(owner, review.toString())
        val restarted = PurchaseAttempts(disk)
        val restored = restarted.recoveryState(owner)!!
        assertEquals("needs_review", restored.payment!!.creditState)
        assertNull(restored.createAck) // no persisted explicit browser acknowledgement
        assertNull(CheckoutOpenPolicy().autoOpenAfterPay(restored.createAck))
        assertNull(CheckoutOpenPolicy().onContinueRequested(restored.payment))
        assertFalse(restored.freshMeConfirmed)
        assertFalse(restored.confirmationPlansLoaded)
        val refreshed = PurchaseFlow.plansLoaded(PurchaseFlow.onFreshMe(restored, freshMe()), "fresh", plans())
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION, refreshed.phase)
        assertTrue(PurchaseFlow.blocksNewPurchase(refreshed))
        assertEquals(refreshed, PurchaseFlow.selectMethod(refreshed, "card"))
        assertRejected { restarted.confirm(owner, refreshed.copy(phase = PurchaseFlow.CONFIRMED)) }
        assertFalse(restarted.restart())
    }

    @Test fun otherOrUnverifiedAccountCannotSeeOrReplaySavedOrder() {
        val disk = Disk()
        val first = prepared(disk)
        first.markCreate(owner, quote())
        first.savePayment(owner, paidEvent().toString())
        val restarted = PurchaseAttempts(disk)
        for (account in listOf(null, "another-account")) {
            val masked = restarted.recoveryState(account)!!
            assertNull(masked.quote)
            assertNull(masked.payment)
            assertNull(masked.selectedPlanId)
            assertTrue(PurchaseFlow.blocksNewPurchase(masked))
            assertRejected { restarted.recoveryOperation(account) }
            assertRejected { restarted.retryCreate(account) }
            assertRejected { restarted.savePayment(account, paidEvent().toString()) }
        }
        assertNotNull(restarted.recoveryState(owner)!!.payment)
    }

    @Test fun quoteOnlyAndProvenTerminalAllowDeliberateNewSelection() {
        val disk = Disk()
        var attempts = prepared(disk)
        val quoteOnlyKey = attempts.current()!!.paymentKey
        attempts = PurchaseAttempts(disk)
        assertNull(attempts.recoveryState(null))
        assertNotEquals(quoteOnlyKey, attempts.beginQuote(selection().copy(method = "card")).paymentKey)
        attempts = prepared(disk)
        attempts.markCreate(owner, quote())
        val failed = paidEvent().put("payment_status", "failed").put("credit_state", "unapplied")
            .put("credited_product", JSONObject.NULL).put("credited_entitlement_revision", JSONObject.NULL)
        val terminal = attempts.savePayment(owner, failed.toString())
        val restarted = PurchaseAttempts(disk)
        assertFalse(restarted.current()!!.unresolved)
        assertEquals("terminal", restarted.current()!!.order!!.outcome)
        assertNotEquals(terminal.paymentKey, restarted.beginQuote(selection()).paymentKey)
    }

    @Test fun paidCompletionRequiresFreshMatchingRightsAndPlansBeforeNextPurchase() {
        val disk = Disk()
        val attempts = prepared(disk)
        attempts.markCreate(owner, quote())
        val saved = attempts.savePayment(owner, paidEvent().toString())
        val restarted = PurchaseAttempts(disk)
        val state = restarted.recoveryState(owner)!!
        assertRejected { restarted.confirm(owner, state.copy(phase = PurchaseFlow.CONFIRMED)) }
        val fresh = PurchaseFlow.onFreshMe(state, freshMe())
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION, fresh.phase)
        val confirmed = PurchaseFlow.plansLoaded(fresh, "fresh", plans())
        assertEquals(PurchaseFlow.CONFIRMED, confirmed.phase)
        restarted.confirm(owner, confirmed)
        val after = PurchaseAttempts(disk)
        assertEquals("confirmed", after.current()!!.order!!.outcome)
        assertNull(after.recoveryState(null))
        assertNotEquals(saved.paymentKey, after.beginQuote(selection()).paymentKey)
    }

    @Test fun storageFailureCannotSendPayOrDiscardUnknownOrderAndLegacyIsNotGuessed() {
        val disk = Disk()
        val attempts = prepared(disk)
        disk.fail = true
        var wroteNative = false
        assertEquals(PurchaseSendOutcome.LOCAL_FAILURE, PurchaseSender(PurchaseSingleFlight()).send(
            PurchaseFlight(PurchaseFlightKind.PAYMENT, "native"), {
                attempts.markCreate(owner, quote()); JSONObject()
            }, { wroteNative = true; true }))
        assertFalse(wroteNative)
        disk.fail = false
        attempts.markCreate(owner, quote())
        val saved = disk.bytes
        disk.fail = true
        assertRejected { attempts.savePayment(owner, paidEvent().toString()) }
        assertEquals(saved, disk.bytes)
        disk.fail = false
        disk.bytes = PurchaseAttemptCodec.encode(attempts.current()!!.copy(formatVersion = 1, order = null, quoteEvent = null))
        val legacy = PurchaseAttempts(disk)
        assertEquals("legacy_unknown", legacy.recoveryState(owner)!!.recovery)
        assertFalse(legacy.restart())
        assertRejected { legacy.beginQuote(selection()) }
        disk.bytes = "{broken"
        assertEquals("unreadable", PurchaseAttempts(disk).recoveryState(owner)!!.recovery)
        assertRejected { PurchaseAttempts(disk).beginQuote(selection()) }
    }

    private fun freshMe() = AccountAccessParser.parse(JSONObject("""
        {"v":1,"attempt_id":"restart","type":"account_access","access_version":1,
         "server_time":"2026-10-02T15:10:00Z","session_generation":"2",
         "previous_session_generation":"1","access_revision":"9",
         "account":{"state":"ACTIVE_PAID","telegram_linked":true,"binding_status":"active",
            "management_only":false,"account_ref":"account-recovery-test"},
         "entitlement":{"type":"paid","status":"active","valid_from":"2026-10-03T15:07:49Z",
            "valid_until":"2026-11-02T15:07:49Z","effective_device_limit":4,"slots_used":1,
            "revision":"9","perpetual_commercial":false},
         "onboarding":{"state":"not_started","started_by":"server_confirmed_first_connection",
            "started_at":null,"not_after":null,"duration_seconds":3600,"one_time":true,
            "extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
            "requires_hardware_id":false,"unit":"installation_fingerprint",
            "post_telegram_identity":"account_history_correlation",
            "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
         "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
            "data_access":"subscription_data","effective_deadline":"2026-11-02T15:07:49Z"}}
    """.trimIndent()))
}

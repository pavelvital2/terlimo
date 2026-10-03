package xyz.terlimo.test

import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

/** Uses the same result boundary as SessionService, after its persist/send unknown_create step. */
class PurchaseResultAcceptanceTest {
    private val owner = "acceptance-owner"
    private val installation = "a".repeat(64)
    private val attempt = "acceptance-native"
    private val newQuote = "00000000-0000-4000-8000-000000000022"
    private val newPayment = "00000000-0000-4000-8000-000000000023"

    private class Disk : PurchaseAttemptStore {
        var state = JSONObject().put("unrelated", "preserved")
        var failWrite = false
        var resolutions = 0
        override fun read(): String? = state.opt("purchase_attempt_state") as? String
        override fun write(encoded: String) {
            check(!failWrite) { "write failed" }
            state.put("purchase_attempt_state", encoded)
        }
        override fun resolveNoOrder(expected: String, resolved: String, installationId: String) {
            val next = PurchaseNoOrderResolution.prepare(state, expected, resolved, installationId)
            check(!failWrite) { "write failed" }
            state = next
            resolutions++
        }
    }
    private fun event(type: String): JSONObject {
        val rows = JSONArray(javaClass.getResource("/payment-reviewed-bridge.json")!!.readText())
        return (0 until rows.length()).map { rows.getJSONObject(it) }.first {
            it.getString("scenario") == "selected_renewal" && it.getJSONObject("event").getString("type") == type
        }.getJSONObject("event").put("attempt_id", attempt)
    }
    private fun receipt(status: String, type: String = PaymentsContract.TYPE_PAYMENT_CREATE_RESULT) =
        event(PaymentsContract.TYPE_PAYMENT_GET_RESULT).put("type", type).put("payment_id", newPayment)
            .put("payment_status", status).apply {
                if (status != "paid") put("credit_state", "unapplied").put("credit_review_reason", JSONObject.NULL)
                    .put("credited_product", JSONObject.NULL).put("credited_entitlement_revision", JSONObject.NULL)
                put("checkout_reference", "https://pay.example/current")
            }
    private data class Context(val disk: Disk, val journal: PurchaseAttempts, val tracker: PaymentCreateTracker,
        val host: PurchaseResultAcceptance, val flight: PurchaseFlight, val state: PurchaseState,
        val oldPayment: PaymentStatusView)

    private fun prepared(oldStatus: String = "expired"): Context {
        val disk = Disk()
        val journal = PurchaseAttempts(disk)
        val tracker = PaymentCreateTracker(1000)
        val q1 = event(PaymentsContract.TYPE_QUOTE_CREATE_RESULT)
        val quote1 = (PaymentsContract.parse(q1) as PaymentsEvent.Quote).quote
        val plans = PaymentsContract.parse(event(PaymentsContract.TYPE_PLANS_LIST_RESULT)) as PaymentsEvent.Plans
        val selection = PurchaseSelection.fromPlan(plans.plans.first { it.planId == quote1.product!!.planId },
            quote1.method, quote1.product!!.renewExtraSlotIds)!!
        journal.beginQuote(selection)
        journal.bindQuote(quote1.quoteId, q1.toString())
        journal.markCreate(owner, quote1)
        val oldEvent = receipt(oldStatus).put("payment_id", "00000000-0000-4000-8000-000000000011")
        val old = journal.savePayment(owner, oldEvent.toString()).order!!.payment!!
        assertEquals("terminal", journal.current()!!.order!!.outcome)
        var view = PurchaseState(phase = PurchaseFlow.ERROR, ownerAccountRef = owner, payment = old)
        view = PurchaseFlow.plansLoaded(view, plans.plansRevision, plans.plans)
        view = PurchaseFlow.selectMethod(view, selection.method)
        view = PurchaseFlow.selectRenewExtraSlots(view, selection.renewExtraSlotIds)
        val q2 = JSONObject(q1.toString()).put("quote_id", newQuote)
        val quote2 = (PaymentsContract.parse(q2) as PaymentsEvent.Quote).quote
        journal.beginQuote(selection)
        journal.bindQuote(newQuote, q2.toString())
        view = PurchaseFlow.quoteReady(view, quote2, selection)
        assertEquals(old, view.payment)
        assertEquals(newQuote, view.quote!!.quoteId)
        // Exact service Pay transition: durable intent, live flight binding, then unknown UI.
        val record = journal.markCreate(owner, quote2)
        tracker.onSent(newQuote)
        val flight = PurchaseFlight(PurchaseFlightKind.PAYMENT, attempt, newQuote,
            record.paymentKey, record, installation)
        view = view.copy(recovery = "unknown_create", ownerAccountRef = record.order!!.accountRef)
        assertTrue(PurchaseFlow.blocksNewPurchase(view))
        assertNull(record.order.payment)
        return Context(disk, journal, tracker, PurchaseResultAcceptance(journal, tracker), flight, view, old)
    }
    private fun accept(c: Context, e: JSONObject) = c.host.payment(c.state, c.flight, attempt, owner, installation, e)
    private fun proof(c: Context, reason: String) = JSONObject().put("v", 1).put("attempt_id", attempt)
        .put("type", PaymentsContract.TYPE_PAYMENT_CREATE_RESULT).put("state", "error")
        .put("code", if (reason == "referral_discount_reserved") "REFERRAL_DISCOUNT_RESERVED" else "QUOTE_EXPIRED")
        .put("reason", reason).put("http_status", 409).put("retryable", false)
        .put("request_id", "0123456789abcdef0123456789abcdef")
        .put("create_resolution", JSONObject().put("kind", "no_order").put("quote_id", newQuote)
            .put("request_idempotency_key", c.flight.idempotencyKey).put("reason", reason))
    private fun rejected(block: () -> Unit) { assertThrows(IllegalStateException::class.java) { block() } }

    @Test fun pendingCreateReplacesDisplayedHistoryOnlyAfterPersistedUnknownAndOpensOnce() {
        for (old in listOf("expired", "failed")) {
            val c = prepared(old)
            val view = accept(c, receipt("pending"))
            assertEquals(newPayment, view.payment!!.paymentId)
            assertEquals(c.journal.current()!!.order!!.payment, view.payment)
            assertEquals("known_payment", view.recovery)
            val ack = view.createAck!!
            assertEquals(newQuote, ack.quoteId)
            val policy = CheckoutOpenPolicy()
            policy.onPayRequested(newQuote)
            assertEquals(newPayment, policy.autoOpenAfterPay(ack)!!.paymentId)
            assertNull(policy.autoOpenAfterPay(ack))
            rejected { accept(c, receipt("pending")) }
            assertEquals(newPayment, c.journal.current()!!.order!!.payment!!.paymentId)
        }
    }

    @Test fun paidAndTerminalCreateUseTheSameAcceptedCurrentIntentWithoutAutoOpen() {
        for (status in listOf("paid", "expired", "failed")) {
            val c = prepared()
            val view = accept(c, receipt(status))
            assertEquals(newPayment, view.payment!!.paymentId)
            assertEquals(status, view.payment.paymentStatus)
            assertNull(view.createAck)
            val policy = CheckoutOpenPolicy().also { it.onPayRequested(newQuote) }
            assertNull(policy.autoOpenAfterPay(view.createAck))
            assertEquals(if (status == "paid") PurchaseFlow.AWAITING_CONFIRMATION else PurchaseFlow.ERROR, view.phase)
            assertEquals(if (status == "paid") "unresolved" else "terminal", c.journal.current()!!.order!!.outcome)
            assertEquals(if (status == "paid") "known_payment" else null, view.recovery)
        }
    }

    @Test fun noOrderIgnoresUnrelatedDisplayedReceiptAndCommitsHistoryOnce() {
        for (reason in listOf("referral_discount_reserved", "referral_quote_changed")) {
            val c = prepared("failed")
            val original = c.disk.read()
            val e = proof(c, reason)
            val view = c.host.noOrder(c.state, c.flight, attempt, owner, installation, e)
            assertEquals(PurchaseFlow.NO_ORDER, view.phase)
            assertNull(view.payment)
            assertNull(view.createAck)
            assertFalse(PurchaseFlow.blocksNewPurchase(view))
            assertEquals("referral_no_order", c.journal.current()!!.order!!.outcome)
            assertEquals(original, c.disk.state.getJSONObject("purchase_resolution_history")
                .getJSONObject(c.flight.paymentIntent!!.attemptId).getString("original"))
            assertEquals(view, c.host.noOrder(c.state, c.flight, attempt, owner, installation, e))
            assertEquals(1, c.disk.resolutions)
            assertEquals("preserved", c.disk.state.getString("unrelated"))
        }
    }

    @Test fun failedWritesNeverPublishNewReceiptAckOrNoOrderRelease() {
        for (noOrder in listOf(false, true)) {
            val c = prepared()
            val before = c.disk.read()
            c.disk.failWrite = true
            rejected {
                if (noOrder) c.host.noOrder(c.state, c.flight, attempt, owner, installation,
                    proof(c, "referral_quote_changed")) else accept(c, receipt("pending"))
            }
            assertEquals(before, c.disk.read())
            assertEquals(c.oldPayment, c.state.payment)
            assertTrue(PurchaseFlow.blocksNewPurchase(c.state))
            assertNull(c.state.createAck)
            assertEquals(0, c.disk.resolutions)
            // The failed save did not consume the pending explicit create acknowledgement.
            if (!noOrder) { c.disk.failWrite = false; assertNotNull(accept(c, receipt("pending")).createAck) }
        }
    }

    @Test fun foreignGetCannotReplaceHistoricalOrCurrentSavedReceipt() {
        val c = prepared()
        val get = PurchaseFlight(PurchaseFlightKind.PAYMENT_GET, attempt)
        val foreign = receipt("pending", PaymentsContract.TYPE_PAYMENT_GET_RESULT)
            .put("payment_id", "00000000-0000-4000-8000-000000000099")
        val before = c.disk.read()
        rejected { c.host.payment(c.state, get, attempt, owner, installation, foreign) }
        assertEquals(before, c.disk.read())
        assertEquals(c.oldPayment, c.state.payment)
        val accepted = accept(c, receipt("pending"))
        val stored = c.disk.read()
        rejected { c.host.payment(accepted, get, attempt, owner, installation, foreign) }
        assertEquals(stored, c.disk.read())
        assertEquals(newPayment, accepted.payment!!.paymentId)
        val refreshed = c.host.payment(accepted, get, attempt, owner, installation,
            receipt("pending", PaymentsContract.TYPE_PAYMENT_GET_RESULT))
        assertEquals(newPayment, refreshed.payment!!.paymentId)
        assertEquals(accepted.createAck, refreshed.createAck)
    }

    @Test fun createRequiresExactCapturedFlightAccountInstallationAndDiskIntent() {
        val c = prepared()
        val before = c.disk.read()
        for (bad in listOf(c.flight.copy(kind = PurchaseFlightKind.PAYMENT_GET),
            c.flight.copy(attempt = "foreign"), c.flight.copy(quoteId = "foreign"),
            c.flight.copy(idempotencyKey = "foreign"), c.flight.copy(installationId = "b".repeat(64)),
            c.flight.copy(paymentIntent = null))) {
            rejected { c.host.payment(c.state, bad, attempt, owner, installation, receipt("pending")) }
        }
        rejected { c.host.payment(c.state, c.flight, attempt, "foreign-owner", installation, receipt("pending")) }
        rejected { c.host.payment(c.state, c.flight, attempt, owner, installation,
            receipt("pending").put("attempt_id", "foreign")) }
        assertEquals(before, c.disk.read())
        val captured = c.flight.paymentIntent!!
        c.disk.write(PurchaseAttemptCodec.encode(captured.copy(paymentKey = "different-payment-key")))
        val changed = c.disk.read()
        rejected { accept(c, receipt("pending")) }
        assertEquals(changed, c.disk.read())
    }
}

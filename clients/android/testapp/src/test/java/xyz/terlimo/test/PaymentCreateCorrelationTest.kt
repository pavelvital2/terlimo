package xyz.terlimo.test

import java.io.File
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Focused regression of the local create acknowledgement: the wire carries no quote id on
 * payment results ([PaymentStatusView] has none), so only the recorded explicit send of an
 * exact quote may turn a `payment_create_result` into a [PaymentCreateAck]. A
 * `payment_get_result`, an unsolicited create result, a duplicate result, a cleared attempt
 * and an already-consumed generation never can. The chain test proves that an old order kept
 * in [PurchaseState] (or refreshed by a get) can never satisfy the explicit «Оплатить»
 * marker: only the matched create ack of the sent quote opens, exactly once, and an attempt
 * failure or replacement drops the controlled correlation.
 */
class PaymentCreateCorrelationTest {
    private val create = PaymentsContract.TYPE_PAYMENT_CREATE_RESULT
    private val get = PaymentsContract.TYPE_PAYMENT_GET_RESULT

    private fun payment(id: String, reference: String?, status: String = "created") =
        PaymentStatusView(
            paymentId = id, paymentStatus = status, checkoutReference = reference,
            creditedEntitlementRevision = null, accessApplicationState = "not_requested",
        )

    private fun plan(planId: String, duration: String) =
        PaymentPlan(planId, "Подписка", duration, 2, 30_000, "RUB", listOf("card", "sbp"))

    private fun quote(quoteId: String, duration: String, method: String) =
        PaymentQuote(quoteId, 30_000, "RUB", duration, 2, method, "2026-09-30T15:35:00Z")

    private fun source(path: String): String = listOf(File(path), File("testapp/$path"))
        .first { it.isFile }.readText()

    @Test
    fun onlyTheCreateResultOfTheRecordedSendBuildsTheAck() {
        val tracker = PaymentCreateTracker(seed = 100L)
        val created = payment("pay-1", "https://pay.example/s/1")
        // No explicit send was recorded: even a create result is unsolicited and acks nothing.
        assertNull(tracker.onCreateResult(create, created))
        tracker.onSent("quote-1")
        // A get result never acks and never consumes the pending send.
        assertNull(tracker.onCreateResult(get, created))
        val ack = tracker.onCreateResult(create, created)
        assertNotNull(ack)
        assertEquals("quote-1", ack!!.quoteId)
        assertEquals(101L, ack.seq)
        assertEquals("pay-1", ack.payment.paymentId)
        // One send acknowledges exactly one create result; a duplicate is not a second ack.
        assertNull(tracker.onCreateResult(create, created))
    }

    @Test
    fun aGetResultNeverConsumesThePendingSend() {
        val tracker = PaymentCreateTracker(seed = 500L)
        tracker.onSent("quote-1")
        assertNull(tracker.onCreateResult(get, payment("pay-old", "https://pay.example/s/old", "pending")))
        // The pending send survived the get result: its own create result still acks.
        val ack = tracker.onCreateResult(create, payment("pay-1", "https://pay.example/s/1"))
        assertNotNull(ack)
        assertEquals("quote-1", ack!!.quoteId)
        assertEquals(501L, ack.seq)
    }

    @Test
    fun clearDropsThePendingSendOfAnAbandonedAttempt() {
        val tracker = PaymentCreateTracker(seed = 10L)
        tracker.onSent("quote-1")
        tracker.clear()
        assertNull(tracker.onCreateResult(create, payment("pay-1", "https://pay.example/s/1")))
        // After the clear a later explicit send is tracked normally again.
        tracker.onSent("quote-1")
        val ack = tracker.onCreateResult(create, payment("pay-1", "https://pay.example/s/1"))
        assertNotNull(ack)
        assertEquals(12L, ack!!.seq)
    }

    @Test
    fun aFreshTrackerNeverRepeatsAGenerationConsumedBeforeRecreation() {
        val before = PaymentCreateTracker(seed = 1_000L)
        before.onSent("quote-1")
        val consumed = before.onCreateResult(create, payment("pay-1", "https://pay.example/s/1"))
        assertNotNull(consumed)
        assertEquals(1_001L, consumed!!.seq)

        val policy = CheckoutOpenPolicy()
        policy.onPayRequested("quote-1")
        val firstOpen = policy.autoOpenAfterPay(consumed)
        assertNotNull(firstOpen)
        policy.onOpened(firstOpen!!.paymentId)
        val restored = CheckoutOpenPolicy()
        restored.restore(policy.savedState())
        assertEquals(1_001L, restored.savedState().lastConsumedAckSeq)

        // A fresh tracker seeded after recreation can only mint a greater generation, so the
        // persisted consumed one is never re-consumed; only the new create opens again.
        val after = PaymentCreateTracker(seed = 2_000L)
        after.onSent("quote-1")
        val next = after.onCreateResult(create, payment("pay-2", "https://pay.example/s/2"))
        assertNotNull(next)
        assertEquals(2_001L, next!!.seq)
        restored.onPayRequested("quote-1")
        val secondOpen = restored.autoOpenAfterPay(next)
        assertNotNull(secondOpen)
        assertEquals("pay-2", secondOpen!!.paymentId)
    }

    @Test
    fun pureProjectionCannotReplaceAnOldOrderWithoutDurableAcceptance() {
        val tracker = PaymentCreateTracker(seed = 1_000L)
        val policy = CheckoutOpenPolicy()
        val oldPending = payment("pay-old", "https://pay.example/s/old", "pending")
        val oldTerminal = payment("pay-old", "https://pay.example/s/old", "expired")
        val fresh = payment("pay-new", "https://pay.example/s/new", "pending")

        // An old pending order is already in ViewState; the user changes plan and method and
        // receives the new quote Q2. The old order stays visible but carries no create ack.
        var state = PurchaseFlow.quoteReady(
            PurchaseFlow.plansLoaded(null, "5", listOf(plan("p30", "days:30"), plan("p93", "months:3"))),
            quote("q-1", "days:30", "card"))
        state = PurchaseFlow.paymentGetResult(state, oldPending)
        assertEquals("pay-old", state.payment?.paymentId)
        state = PurchaseFlow.selectPlan(state, "p93")
        state = PurchaseFlow.selectMethod(state, "sbp")
        state = PurchaseFlow.quoteReady(state, quote("q-2", "months:3", "sbp"))
        assertEquals("pay-old", state.payment?.paymentId)
        assertNull(state.createAck)

        // Explicit «Оплатить» for Q2: the host arms the marker and records the sent create.
        policy.onPayRequested("q-2")
        tracker.onSent("q-2")

        // While the create result is in flight the old order is refreshed by a get render:
        // it neither opens nor spends the live marker, and the old terminal state is no
        // better. The awaited create ack can still open afterwards.
        state = PurchaseFlow.paymentGetResult(state, oldPending)
        assertNull(state.createAck)
        assertNull(policy.autoOpenAfterPay(state.createAck))
        state = PurchaseFlow.paymentGetResult(state, oldTerminal)
        assertNull(state.createAck)
        assertNull(policy.autoOpenAfterPay(state.createAck))

        // Only the correlated create result of the exact sent quote may open; it opens the
        // created payment once, and the same generation cannot open again.
        val ack = tracker.onCreateResult(create, fresh)
        assertNotNull(ack)
        assertEquals("q-2", ack!!.quoteId)
        // The pure projection cannot authorize replacing a displayed receipt. The
        // service acceptance boundary must first prove and durably accept the new intent.
        state = PurchaseFlow.paymentCreateResult(state, fresh, ack)
        assertNull(state.createAck)
        assertNull(policy.autoOpenAfterPay(state.createAck))
        assertEquals("pay-old", state.payment?.paymentId)

        // A failure of the attempt drops the controlled correlation immediately.
        assertNull(PurchaseFlow.failure(state, "PROVIDER_UNAVAILABLE").createAck)
    }

    @Test
    fun aReplacedAttemptDropsTheControlledCreateCorrelation() {
        val loaded = PurchaseFlow.plansLoaded(
            null, "5", listOf(plan("p30", "days:30"), plan("p93", "months:3")))
        val created = payment("pay-1", "https://pay.example/s/1")
        val acked = PurchaseFlow.paymentCreateResult(loaded, created, PaymentCreateAck("q-1", 7L, created))
        assertNotNull(acked.createAck)
        // An unchanged selection (a plans refresh of the same attempt) keeps it...
        assertNotNull(PurchaseFlow.plansLoaded(acked, "6", listOf(plan("p30", "days:30"))).createAck)
        // ...while a changed plan/method, a replaced/expired quote and any failure drop it.
        assertNull(PurchaseFlow.selectPlan(acked, "p93").createAck)
        assertNull(PurchaseFlow.selectMethod(acked, "sbp").createAck)
        assertNull(PurchaseFlow.quoteReady(acked, quote("q-2", "days:30", "card")).createAck)
        assertNull(PurchaseFlow.quoteExpiredState(acked).createAck)
        assertNull(PurchaseFlow.failure(acked, "TRANSPORT").createAck)
        assertNull(PurchaseFlow.plansFailure(acked, "TRANSPORT").createAck)
        // A GET for another order cannot replace the original receipt or retain a
        // checkout acknowledgement after the receipt mismatch.
        val foreign = PurchaseFlow.paymentGetResult(
            acked, payment("pay-2", "https://pay.example/s/2", "pending"))
        assertNull(foreign.createAck)
        assertEquals(created, foreign.payment)
        assertEquals("PAYMENT_STATE_INVALID", foreign.error)
        assertNotNull(PurchaseFlow.paymentGetResult(acked, created).createAck)
    }

    @Test
    fun onePurchaseRequestOccupiesTheStreamAtATime() {
        val gate = PurchaseSingleFlight()
        val created = PurchaseFlight(PurchaseFlightKind.PAYMENT, "attempt-1", "q-2", "key-2")
        assertTrue(gate.acquire(created))
        assertEquals(created, gate.holder())
        // A second create, a new quote and a status get are all refused while it is outstanding.
        assertFalse(gate.acquire(PurchaseFlight(PurchaseFlightKind.PAYMENT, "attempt-1", "q-3", "key-3")))
        assertFalse(gate.acquire(PurchaseFlight(PurchaseFlightKind.QUOTE, "attempt-1")))
        assertFalse(gate.acquire(PurchaseFlight(PurchaseFlightKind.PAYMENT_GET, "attempt-1")))
        // A GET result cannot release a create holder; only its own create result can.
        assertFalse(gate.releaseOn(get))
        assertTrue(gate.busy())
        assertTrue(gate.releaseOn(create))
        assertFalse(gate.busy())
        // After the result the stream accepts the next quote and its own create.
        assertTrue(gate.acquire(PurchaseFlight(PurchaseFlightKind.QUOTE, "attempt-1")))
        assertTrue(gate.releaseOn(PaymentsContract.TYPE_QUOTE_CREATE_RESULT))
        assertTrue(gate.acquire(PurchaseFlight(PurchaseFlightKind.PAYMENT, "attempt-1", "q-3", "key-3")))
        // Teardown drops any outstanding request.
        gate.reset()
        assertFalse(gate.busy())
    }

    @Test
    fun aDelayedCreateIsBoundToItsOwnQuoteAndNeverOpensAsTheNext() {
        val gate = PurchaseSingleFlight()
        val tracker = PaymentCreateTracker(seed = 100L)
        val policy = CheckoutOpenPolicy()
        // Q2 create sent and still unanswered.
        assertTrue(gate.acquire(PurchaseFlight(PurchaseFlightKind.PAYMENT, "attempt-1", "q-2")))
        tracker.onSent("q-2")
        // A new quote Q3 and its queued Pay are refused: no second send and no key rotation.
        assertFalse(gate.acquire(PurchaseFlight(PurchaseFlightKind.QUOTE, "attempt-1")))
        assertFalse(gate.acquire(PurchaseFlight(PurchaseFlightKind.PAYMENT, "attempt-1", "q-3")))
        // The delayed Q2 result is bound to Q2 — it can never be labelled Q3.
        val ack2 = tracker.onCreateResult(create, payment("pay-2", "https://pay.example/s/2"))
        assertNotNull(ack2)
        assertEquals("q-2", ack2!!.quoteId)
        assertTrue(gate.releaseOn(create))
        // Armed only for Q2, it opens the Q2 order once and is never re-opened.
        policy.onPayRequested("q-2")
        val open2 = policy.autoOpenAfterPay(ack2)
        assertNotNull(open2)
        assertEquals("pay-2", open2!!.paymentId)
        assertNull(policy.autoOpenAfterPay(ack2))
        // An armed Q3 never accepts the Q2 acknowledgement.
        val other = CheckoutOpenPolicy()
        other.onPayRequested("q-3")
        assertNull(other.autoOpenAfterPay(ack2))
        // Only after completion may Q3 be sent, and its own response opens it.
        assertTrue(gate.acquire(PurchaseFlight(PurchaseFlightKind.QUOTE, "attempt-1")))
        assertTrue(gate.releaseOn(PaymentsContract.TYPE_QUOTE_CREATE_RESULT))
        assertTrue(gate.acquire(PurchaseFlight(PurchaseFlightKind.PAYMENT, "attempt-1", "q-3")))
        tracker.onSent("q-3")
        val ack3 = tracker.onCreateResult(create, payment("pay-3", "https://pay.example/s/3"))
        assertNotNull(ack3)
        assertEquals("q-3", ack3!!.quoteId)
        val open3 = other.autoOpenAfterPay(ack3)
        assertNotNull(open3)
        assertEquals("pay-3", open3!!.paymentId)
    }

    @Test
    fun anOutstandingRequestShowsWaitingAndOpensNothing() {
        val state = PurchaseFlow.sending(
            PurchaseFlow.plansLoaded(null, "5", listOf(plan("p30", "days:30"))))
        assertTrue(state.sending)
        assertEquals(PaymentsText.PURCHASE_SENDING_TEXT,
            PaymentsText.purchaseStatus(state, null, java.time.ZoneId.of("UTC")))
        // A sending state carries no create ack, so the explicit marker cannot open.
        val policy = CheckoutOpenPolicy()
        policy.onPayRequested("q-1")
        assertNull(policy.autoOpenAfterPay(state.createAck))
        // The first parsed result (any of result/get/failure) clears the waiting flag.
        assertFalse(PurchaseFlow.quoteReady(state, quote("q-1", "days:30", "card")).sending)
        assertFalse(PurchaseFlow.paymentGetResult(state, payment("pay-1", null)).sending)
        assertFalse(PurchaseFlow.failure(state, "TRANSPORT").sending)
    }

    @Test
    fun theGateCountsOneSendAndRotatesNoKeyWhileTheCreateIsOutstanding() {
        val store = object : PurchaseAttemptStore {
            private var value: String? = null
            override fun read(): String? = value
            override fun write(encoded: String) { value = encoded }
        }
        var keySeq = 0
        val attempts = PurchaseAttempts(store) { keySeq++; "terlimo-key-%06d".format(keySeq) }
        val gate = PurchaseSingleFlight()
        var paymentsSent = 0
        var quotesSent = 0

        // Explicit Pay for Q2: the durable payment key is rotated once and exactly one
        // payment_create reaches the native stream.
        attempts.beginPayment("q-2")
        val afterQ2Keys = keySeq
        assertTrue(gate.acquire(PurchaseFlight(PurchaseFlightKind.PAYMENT, "a", "q-2")))
        paymentsSent++

        // A queued second Pay for Q3 and a new Quote while the create is unanswered are both
        // refused: no new send and no durable key rotation.
        assertFalse(gate.acquire(PurchaseFlight(PurchaseFlightKind.PAYMENT, "a", "q-3")))
        assertFalse(gate.acquire(PurchaseFlight(PurchaseFlightKind.QUOTE, "a")))
        assertEquals(1, paymentsSent)
        assertEquals(0, quotesSent)
        assertEquals(afterQ2Keys, keySeq)

        // The matching create result releases the stream; only now may a new quote rotate a
        // key and be sent, followed by its own payment_create.
        assertTrue(gate.releaseOn(create))
        assertTrue(gate.acquire(PurchaseFlight(PurchaseFlightKind.QUOTE, "a")))
        attempts.beginQuote("p93", "months:3", "sbp")
        quotesSent++
        assertEquals(afterQ2Keys + 2, keySeq)
        assertTrue(gate.releaseOn(PaymentsContract.TYPE_QUOTE_CREATE_RESULT))
        assertTrue(gate.acquire(PurchaseFlight(PurchaseFlightKind.PAYMENT, "a", "q-3")))
        attempts.beginPayment("q-3")
        paymentsSent++
        assertEquals(2, paymentsSent)
        assertEquals(1, quotesSent)
        assertEquals(afterQ2Keys + 4, keySeq)
    }

    @Test
    fun aFailedOrClosedWriteNeverLeavesAHiddenBusyHolderNorClaimsSent() {
        val gate = PurchaseSingleFlight()
        val sender = PurchaseSender(gate)
        var writes = 0

        // A closed stream returns false from the truthful write: the outcome is unknown, the
        // holder is KEPT (no release over a possibly partial write) and nothing claims a send.
        assertEquals(PurchaseSendOutcome.UNKNOWN_WRITE,
            sender.send(PurchaseFlight(PurchaseFlightKind.PAYMENT, "a", "q-1"), { JSONObject() }, { writes++; false }))
        assertEquals(1, writes)
        assertTrue(gate.busy())
        assertNull(gate.holder()?.idempotencyKey)
        // The standard terminal teardown/epoch fence drops the holder — no eternal busy.
        gate.reset()
        assertFalse(gate.busy())

        // A thrown BRIDGE_CLOSED is the same unknown outcome, never an escaped exception.
        assertEquals(PurchaseSendOutcome.UNKNOWN_WRITE,
            sender.send(PurchaseFlight(PurchaseFlightKind.PAYMENT, "a", "q-1"), { JSONObject() },
                { throw RuntimeException("BRIDGE_CLOSED") }))
        assertTrue(gate.busy())
        gate.reset()

        // A local preparation error frees the capture at once: nothing reached the wire.
        assertEquals(PurchaseSendOutcome.LOCAL_FAILURE,
            sender.send(PurchaseFlight(PurchaseFlightKind.PAYMENT, "a", "q-1"),
                { throw RuntimeException("local") }, { writes++; true }))
        assertFalse(gate.busy())

        // A truthful write reports WRITTEN and keeps the request outstanding.
        assertEquals(PurchaseSendOutcome.WRITTEN,
            sender.send(PurchaseFlight(PurchaseFlightKind.PAYMENT, "a", "q-1", "key-1"), { JSONObject() }, { true }))
        assertTrue(gate.busy())

        // While outstanding, a queued second send is WAITING and writes nothing.
        val before = writes
        assertEquals(PurchaseSendOutcome.WAITING,
            sender.send(PurchaseFlight(PurchaseFlightKind.PAYMENT, "a", "q-2"), { JSONObject() }, { writes++; true }))
        assertEquals(before, writes)
        gate.reset()
        assertFalse(gate.busy())
    }

    @Test
    fun aLocalPreparationFailureFreesTheCaptureEvenAfterTheKeyWasAttached() {
        val gate = PurchaseSingleFlight()
        val sender = PurchaseSender(gate)
        val request = PurchaseFlight(PurchaseFlightKind.PAYMENT, "a", "q-1")
        var writes = 0
        // The real preparation order: the durable key is attached to the SAME capture and only
        // then a local error is thrown — the capture must still be freed (no hidden busy).
        val outcome = sender.send(
            request = request,
            prepare = {
                gate.attachKey(request, "key-1")
                assertEquals("key-1", gate.holder()?.idempotencyKey)
                throw RuntimeException("local")
            },
            write = { writes++; true },
        )
        assertEquals(PurchaseSendOutcome.LOCAL_FAILURE, outcome)
        assertEquals(0, writes)
        assertFalse(gate.busy())
        assertNull(gate.holder())
    }

    @Test
    fun aStaleCaptureAfterResetNeverReleasesTheNewOwner() {
        val gate = PurchaseSingleFlight()
        val old = PurchaseFlight(PurchaseFlightKind.PAYMENT, "a", "q-1")
        assertTrue(gate.acquire(old))
        gate.reset()
        // A new owner with equal kind/attempt/quoteId but a different identity.
        val fresh = PurchaseFlight(PurchaseFlightKind.PAYMENT, "a", "q-1", "key-9")
        assertTrue(gate.acquire(fresh))
        gate.attachKey(fresh, "key-10")
        // Releasing the stale capture (identity, not value) must not free the new owner.
        gate.release(old)
        assertTrue(gate.busy())
        assertEquals("key-10", gate.holder()?.idempotencyKey)
        // A non-matching result type neither frees it; the matching one clears both parts.
        assertFalse(gate.releaseOn(get))
        assertTrue(gate.busy())
        assertTrue(gate.releaseOn(create))
        assertFalse(gate.busy())
        assertNull(gate.holder())
    }

    @Test
    fun theServiceWiresTheCorrelationOnlyAroundTheExplicitSend() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        val sender = service.substringAfter("private fun sendPurchaseOperation(")
            .substringBefore("private fun handlePurchaseEvent(")
        assertTrue(sender.contains("paymentCreates.onSent(operation.quoteId)"))
        assertTrue(sender.contains("if (before?.attemptId != record.attemptId) paymentCreates.clear()"))
        // Single-flight capture happens before any durable key is rotated: the capture lives in
        // PurchaseSender.send and the durable begin*/keys live only in the prepare lambda.
        assertTrue(sender.contains("val connection = native ?: return"))
        assertTrue(sender.contains("val outcome = purchaseSender.send("))
        assertTrue(sender.contains("prepare = {"))
        assertTrue(sender.indexOf("purchaseSender.send(") < sender.indexOf("beginQuote("))
        assertTrue(sender.indexOf("purchaseSender.send(") < sender.indexOf("markCreate("))
        val capture = source("src/main/java/xyz/terlimo/test/PurchaseSender.kt")
        assertTrue(capture.contains("if (!flight.acquire(request)) return PurchaseSendOutcome.WAITING"))
        assertTrue(capture.indexOf("flight.acquire(request)") < capture.indexOf("prepare()"))
        assertTrue(sender.contains("write = { connection.trySend(it) }"))
        // The old throwing send path must not be used: its exception would be swallowed by the
        // BridgeActor executor and leave the single-flight holder busy forever.
        assertFalse(sender.contains("connection.send(message)"))
        // An unknown/partial write is fenced by the standard terminal teardown, not by an
        // unguarded release of the holder.
        assertTrue(sender.contains("terminalFailure(attempt, \"BRIDGE_WRITE_UNKNOWN\")"))
        assertTrue(sender.contains("purchaseFlight.attachKey(flight, record.quoteKey)"))
        assertTrue(sender.contains("purchaseFlight.attachKey(flight, record.paymentKey)"))

        val handler = service.substringAfter("private fun handlePurchaseEvent(")
            .substringBefore("private fun armPurchaseConfirmationWindow(")
        assertTrue(handler.contains("purchaseResults.payment(view.purchase, holder, attempt, purchaseVerifiedAccountRef,"))
        assertTrue(handler.contains("purchaseResults.noOrder(view.purchase, holder, attempt, purchaseVerifiedAccountRef,"))
        assertFalse(handler.contains("check(view.purchase?.payment == null)"))
        val acceptance = source("src/main/java/xyz/terlimo/test/PurchaseResultAcceptance.kt")
        assertTrue(acceptance.contains("creates.onCreateResult(parsed.type, parsed.payment)"))
        assertTrue(acceptance.indexOf("attempts.savePayment(") < acceptance.indexOf("creates.onCreateResult("))
        assertTrue(handler.contains(
            "if (parsed.type == PaymentsContract.TYPE_PAYMENT_CREATE_RESULT) paymentCreates.clear()"))
        assertTrue(acceptance.contains("if (terminal) creates.clear()"))
        assertFalse(handler.contains("onCreateResult(PaymentsContract.TYPE_PAYMENT_GET_RESULT"))
        // The single-flight holder is released only by the matching parsed result/failure.
        assertTrue(handler.contains("purchaseFlight.releaseOn("))

        val payAction = service.substringAfter("\"purchase_pay\" ->")
            .substringBefore("\"purchase_check\" ->")
        assertTrue(payAction.contains("paymentCreates.clear()"))

        // Attempt teardown drops any pending create correlation and the outstanding flight.
        val stop = service.substringAfter("private fun stopAttempt(").substringBefore("\n    private fun ")
        assertTrue(stop.contains("paymentCreates.clear()"))
        assertTrue(stop.contains("purchaseFlight.reset()"))
        // A queued duplicate tap re-checks the gate under the serial control path.
        val tapHandler = service.substringAfter("private fun handlePurchaseOperation(")
            .substringBefore("private fun sendPurchaseOperation(")
        assertTrue(tapHandler.contains("if (purchaseFlight.busy())"))
    }
}

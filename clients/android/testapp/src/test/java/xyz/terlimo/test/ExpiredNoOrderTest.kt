package xyz.terlimo.test

import java.time.ZoneOffset
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class ExpiredNoOrderTest {
    private val installation = "a".repeat(64)
    private val account = "synthetic-account"
    private val nativeAttempt = "synthetic-native-attempt"
    private class Disk : PurchaseAttemptStore {
        var state = JSONObject().put("identity", "keep").put("other_namespace", JSONObject().put("keep", 1))
            .put("purchase_support_history", JSONObject().put("legacy", "keep"))
        var writes = 0
        var failCommit = false
        var beforeCommit: (() -> Unit)? = null
        override fun read() = state.opt("purchase_attempt_state") as? String
        override fun write(encoded: String) { state.put("purchase_attempt_state", encoded); writes++ }
        override fun resolveNoOrder(expected: String, resolved: String, installationId: String) {
            beforeCommit?.invoke()
            val next = PurchaseNoOrderResolution.prepare(state, expected, resolved, installationId)
            if (failCommit) error("synthetic AtomicFile write failure")
            state = next
            writes++
        }
    }
    private fun event(type: String): JSONObject {
        val rows = JSONArray(javaClass.getResource("/payment-reviewed-bridge.json")!!.readText())
        return (0 until rows.length()).map { rows.getJSONObject(it) }.first {
            it.getString("scenario") == "selected_renewal" && it.getJSONObject("event").getString("type") == type
        }.getJSONObject("event")
    }
    private fun proof() = JSONObject().put("v", 1).put("attempt_id", nativeAttempt)
        .put("type", PaymentsContract.TYPE_PAYMENT_CREATE_RESULT).put("state", "error")
        .put("code", "QUOTE_EXPIRED").put("reason", PaymentsContract.EXPIRED_NO_ORDER_REASON)
    private fun prepare(disk: Disk): Pair<PurchaseAttempts, PurchaseFlight> {
        val attempts = PurchaseAttempts(disk)
        val q = event(PaymentsContract.TYPE_QUOTE_CREATE_RESULT)
        val quote = (PaymentsContract.parse(q) as PaymentsEvent.Quote).quote
        val plans = (PaymentsContract.parse(event(PaymentsContract.TYPE_PLANS_LIST_RESULT)) as PaymentsEvent.Plans).plans
        val selection = PurchaseSelection.fromPlan(plans.first { it.planId == quote.product!!.planId },
            quote.method, quote.product!!.renewExtraSlotIds)!!
        attempts.beginQuote(selection)
        attempts.bindQuote(quote.quoteId, q.toString())
        val record = attempts.markCreate(account, quote)
        return attempts to PurchaseFlight(PurchaseFlightKind.PAYMENT, nativeAttempt, quote.quoteId,
            record.paymentKey, record, installation)
    }
    private fun resolve(attempts: PurchaseAttempts, flight: PurchaseFlight, e: JSONObject = proof()) =
        attempts.resolveExpiredNoOrder(flight, nativeAttempt, account, installation, e.toString())
    private fun rejected(block: () -> Unit) {
        try { block(); fail("expected refusal") } catch (_: IllegalStateException) { }
    }

    @Test fun exactProofCommitsIntentHistoryAndUnlocksOnlyAfterCommitAcrossRestart() {
        val disk = Disk()
        val (attempts, flight) = prepare(disk)
        val original = disk.read()
        val before = attempts.current()!!
        val writes = disk.writes
        disk.beforeCommit = {
            assertTrue(PurchaseFlow.blocksNewPurchase(attempts.recoveryState(account)))
            assertEquals(original, disk.read())
        }
        val resolved = resolve(attempts, flight)
        assertEquals(writes + 1, disk.writes)
        assertFalse(resolved.unresolved)
        assertEquals(before.attemptId, resolved.attemptId)
        assertEquals(before.quoteKey, resolved.quoteKey)
        assertEquals(before.paymentKey, resolved.paymentKey)
        assertEquals(before.quoteEvent, resolved.quoteEvent)
        assertEquals(before.selection, resolved.selection)
        assertEquals(account, resolved.order!!.accountRef)
        assertNull(resolved.order.payment)
        val history = disk.state.getJSONObject("purchase_resolution_history").getJSONObject(before.attemptId)
        assertEquals(original, history.getString("original"))
        assertEquals(resolved, PurchaseAttemptCodec.decode(history.getString("resolved")))
        assertEquals("keep", disk.state.getString("identity"))
        assertEquals(1, disk.state.getJSONObject("other_namespace").getInt("keep"))
        assertEquals("keep", disk.state.getJSONObject("purchase_support_history").getString("legacy"))
        val restarted = PurchaseAttempts(disk) { error("no automatic key rotation") }
        val view = restarted.recoveryState(account)!!
        assertFalse(PurchaseFlow.blocksNewPurchase(view))
        assertNull(view.quote)
        assertNull(view.createAck)
        assertNull(view.payment)
        assertFalse(view.freshMeConfirmed)
        assertEquals("Предложение истекло. Заказ не создан. Можно выбрать тариф заново.",
            PaymentsText.purchaseStatus(view, null, ZoneOffset.UTC))
        assertNull(restarted.recoveryState("other-account"))
        assertNull(restarted.recoveryState(null))
        assertEquals(resolved, resolve(restarted, flight))
        assertEquals(writes + 1, disk.writes)
        rejected { restarted.retryCreate(account) }
        // Only an explicit new quote choice rotates keys; the archived intent is retained.
        disk.beforeCommit = null
        val next = attempts.beginQuote(before.selection!!)
        assertNotEquals(before.attemptId, next.attemptId)
        assertNotEquals(before.paymentKey, next.paymentKey)
        val snapshot = disk.state.toString()
        rejected { resolve(restarted, flight) }
        assertEquals(snapshot, disk.state.toString())
        assertEquals(original, disk.state.getJSONObject("purchase_resolution_history")
            .getJSONObject(before.attemptId).getString("original"))
    }

    @Test fun failedAtomicWriteKeepsOriginalBarrierAndNoHistory() {
        val disk = Disk()
        val (attempts, flight) = prepare(disk)
        val before = disk.state.toString()
        val writes = disk.writes
        disk.failCommit = true
        rejected { resolve(attempts, flight) }
        assertEquals(before, disk.state.toString())
        assertEquals(writes, disk.writes)
        val restart = PurchaseAttempts(disk) { error("do not rotate") }
        assertTrue(PurchaseFlow.blocksNewPurchase(restart.recoveryState(account)))
        assertEquals(flight.paymentIntent, restart.retryCreate(account))
        val retry = restart.recoveryOperation(account) as PurchaseOperation.Payment
        assertEquals(flight.quoteId, retry.quoteId)
        assertTrue(retry.recovery)
    }

    @Test fun interveningWriteCannotBeOverwrittenByResolution() {
        val disk = Disk()
        val (attempts, flight) = prepare(disk)
        val paymentEvent = event(PaymentsContract.TYPE_PAYMENT_GET_RESULT).toString()
        val paid = flight.paymentIntent!!.copy(order = SavedPurchaseOrder(account, paymentEvent))
        disk.beforeCommit = { disk.write(PurchaseAttemptCodec.encode(paid)) }
        rejected { resolve(attempts, flight) }
        assertEquals(paid, attempts.current())
        assertTrue(attempts.current()!!.unresolved)
        assertFalse(disk.state.has("purchase_resolution_history"))
        assertTrue(PurchaseFlow.blocksNewPurchase(attempts.recoveryState(account)))
    }

    @Test fun nonExactErrorsAndMalformedReasonNeverResolve() {
        val disk = Disk()
        val (attempts, flight) = prepare(disk)
        val before = disk.state.toString()
        for (code in listOf("QUOTE_EXPIRED", "TRANSPORT", "SERVICE_UNAVAILABLE", "NOT_FOUND")) {
            val plain = proof().put("code", code).also { it.remove("reason") }
            assertFalse((PaymentsContract.parse(plain) as PaymentsEvent.Failure).expiredNoOrder)
            rejected { resolve(attempts, flight, plain) }
        }
        for (value in listOf<Any>("unknown_reason", 1, JSONObject.NULL, JSONObject().put("reason", "expired_quote_no_order"))) {
            rejected { resolve(attempts, flight, proof().put("reason", value)) }
        }
        for (type in listOf(PaymentsContract.TYPE_PLANS_LIST_RESULT, PaymentsContract.TYPE_QUOTE_CREATE_RESULT,
            PaymentsContract.TYPE_PAYMENT_GET_RESULT)) {
            rejected { resolve(attempts, flight, proof().put("type", type)) }
        }
        rejected { resolve(attempts, flight, proof().put("unexpected", true)) }
        rejected { resolve(attempts, flight, proof().put("code", "SERVICE_UNAVAILABLE")) }
        assertEquals(before, disk.state.toString())
        assertTrue(PurchaseFlow.blocksNewPurchase(attempts.recoveryState(account)))
    }

    @Test fun staleAccountInstallationAttemptKeyAndReceiptFenceResolution() {
        val disk = Disk()
        val (attempts, flight) = prepare(disk)
        val before = disk.state.toString()
        for (bad in listOf(flight.copy(kind = PurchaseFlightKind.PAYMENT_GET),
            flight.copy(attempt = "old-native"), flight.copy(quoteId = "other-quote"),
            flight.copy(idempotencyKey = "other-key"), flight.copy(installationId = "b".repeat(64)),
            flight.copy(paymentIntent = null), flight.copy(paymentIntent = flight.paymentIntent!!.copy(attemptId = "older")))) {
            rejected { resolve(attempts, bad) }
        }
        for (owner in listOf(null, "other-account")) {
            rejected { attempts.resolveExpiredNoOrder(flight, nativeAttempt, owner, installation, proof().toString()) }
        }
        rejected { resolve(attempts, flight, proof().put("attempt_id", "other-native")) }
        assertEquals(before, disk.state.toString())
        val paid = event(PaymentsContract.TYPE_PAYMENT_GET_RESULT).toString()
        attempts.savePayment(account, paid)
        val withReceipt = disk.state.toString()
        rejected { resolve(attempts, flight) }
        assertEquals(withReceipt, disk.state.toString())
        assertTrue(PurchaseFlow.blocksNewPurchase(attempts.recoveryState(account)))
    }

    @Test fun singleFlightCaptureSurvivesReleaseButAccountChangePermanentlyInvalidatesProof() {
        val disk = Disk()
        val (attempts, prepared) = prepare(disk)
        val single = PurchaseSingleFlight()
        val capture = PurchaseFlight(PurchaseFlightKind.PAYMENT, nativeAttempt, prepared.quoteId)
        assertTrue(single.acquire(capture))
        single.attachKey(capture, prepared.idempotencyKey)
        single.attachCreate(capture, prepared.paymentIntent!!, installation)
        assertEquals(prepared, single.holder())
        single.invalidateCreateProof() // account A -> B -> A cannot re-enable this holder
        val fenced = single.holder()!!
        assertNull(fenced.paymentIntent)
        rejected { resolve(attempts, fenced) }
        single.releaseOn(PaymentsContract.TYPE_PAYMENT_CREATE_RESULT)
        assertNull(single.holder())
        assertTrue(PurchaseFlow.blocksNewPurchase(attempts.recoveryState(account)))
    }
}

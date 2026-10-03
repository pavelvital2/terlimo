package xyz.terlimo.test

import java.time.Instant
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

/** Frozen prices and intent barriers are exercised through persisted bytes and fresh journals. */
class PurchaseReferralPaymentTest {
    private val owner = "account-referral-payment"
    private val installation = "a".repeat(64)
    private val nativeAttempt = "referral-native-attempt"
    private val pricing = PaymentPricing(30_167, 10_000, 20_167, "RUB", "referral_first_main", "referral-20261003-v1")

    private class Disk : PurchaseAttemptStore {
        var state = JSONObject().put("other_namespace", "preserved")
        var failCommit = false
        var beforeCommit: (() -> Unit)? = null
        var commits = 0
        override fun read(): String? = state.opt("purchase_attempt_state") as? String
        override fun write(encoded: String) { state.put("purchase_attempt_state", encoded) }
        override fun resolveNoOrder(expected: String, resolved: String, installationId: String) {
            beforeCommit?.invoke()
            val next = PurchaseNoOrderResolution.prepare(state, expected, resolved, installationId)
            if (failCommit) error("atomic write failed")
            state = next
            commits++
        }
    }

    private fun event(type: String): JSONObject {
        val rows = JSONArray(javaClass.getResource("/payment-reviewed-bridge.json")!!.readText())
        return (0 until rows.length()).map { rows.getJSONObject(it) }.first {
            it.getString("scenario") == "selected_renewal" && it.getJSONObject("event").getString("type") == type
        }.getJSONObject("event")
    }

    private fun quoteEvent() = event(PaymentsContract.TYPE_QUOTE_CREATE_RESULT).apply {
        getJSONObject("amount").put("amount_minor", pricing.payableAmountMinor)
        put("pricing", PaymentPricingCodec.encode(pricing))
    }

    private fun quote() = (PaymentsContract.parse(quoteEvent()) as PaymentsEvent.Quote).quote
    private fun plans() = (PaymentsContract.parse(event(PaymentsContract.TYPE_PLANS_LIST_RESULT)) as PaymentsEvent.Plans).plans
    private fun selection(): PurchaseSelection {
        val q = quote()
        return PurchaseSelection.fromPlan(plans().first { it.planId == q.product!!.planId }, q.method,
            q.product!!.renewExtraSlotIds)!!
    }

    private fun paymentEvent(status: String = "paid", discountState: String? = "consumed") =
        event(PaymentsContract.TYPE_PAYMENT_GET_RESULT).apply {
            put("pricing", PaymentPricingCodec.encode(pricing)).put("payment_status", status)
            if (discountState != null) put("referral_discount_state", discountState)
            if (status == "paid") getJSONObject("credited_product").put("pricing", PaymentPricingCodec.encode(pricing))
            else {
                put("credit_state", "unapplied").put("credited_entitlement_revision", JSONObject.NULL)
                    .put("credited_product", JSONObject.NULL)
            }
        }

    private fun prepared(disk: Disk): Pair<PurchaseAttempts, PurchaseFlight> {
        val attempts = PurchaseAttempts(disk)
        attempts.beginQuote(selection())
        attempts.bindQuote(quote().quoteId, quoteEvent().toString())
        val record = attempts.markCreate(owner, quote())
        return attempts to PurchaseFlight(PurchaseFlightKind.PAYMENT, nativeAttempt, record.quoteId,
            record.paymentKey, record, installation)
    }

    private fun proof(flight: PurchaseFlight, reason: String = "referral_discount_reserved") = JSONObject()
        .put("v", 1).put("attempt_id", nativeAttempt).put("type", PaymentsContract.TYPE_PAYMENT_CREATE_RESULT)
        .put("state", "error").put("http_status", 409).put("retryable", false)
        .put("request_id", "0123456789abcdef0123456789abcdef")
        .put("code", if (reason == "referral_discount_reserved") "REFERRAL_DISCOUNT_RESERVED" else "QUOTE_EXPIRED")
        .put("reason", reason).put("create_resolution", JSONObject().put("kind", "no_order")
            .put("quote_id", flight.quoteId).put("request_idempotency_key", flight.idempotencyKey).put("reason", reason))

    private fun resolve(attempts: PurchaseAttempts, flight: PurchaseFlight, event: JSONObject = proof(flight)) =
        attempts.resolveNoOrder(flight, nativeAttempt, owner, installation, event.toString())

    private fun rejected(block: () -> Unit) {
        try { block(); fail("must preserve original intent") } catch (_: IllegalStateException) { }
    }

    @Test fun `discounted quote keeps whole extras period and exact price across create and restart`() {
        val disk = Disk()
        val (attempts, flight) = prepared(disk)
        val q = quote()
        val plan = plans().first { it.planId == q.product!!.planId }
        val selected = PurchaseFlow.selectRenewExtraSlots(PurchaseFlow.plansLoaded(null, "5", plans()),
            q.product!!.renewExtraSlotIds)
        val offered = PurchaseFlow.quoteReady(selected, q)
        assertEquals(q, PurchaseFlow.payableQuote(offered, plan, q.method, Instant.parse("2026-10-02T15:08:00Z")))
        assertEquals(20_000L, offered.quote!!.product!!.baseAmountMinor)
        assertEquals(10_167L, offered.quote.product!!.extraAmountMinor)
        assertEquals(selection().renewExtraSlotIds, offered.quote.product!!.renewExtraSlotIds)
        assertEquals(pricing, offered.quote.pricing)
        val saved = attempts.savePayment(owner, paymentEvent("pending", "reserved").toString())
        val restarted = PurchaseAttempts(disk) { error("never replace original keys") }
        assertEquals(flight.paymentIntent!!.paymentKey, restarted.current()!!.paymentKey)
        assertEquals(q, restarted.current()!!.frozenQuote)
        assertEquals(pricing, restarted.current()!!.order!!.payment!!.pricing)
        assertEquals(PurchaseOperation.PaymentGet(saved.order!!.payment!!.paymentId), restarted.recoveryOperation(owner))
        assertTrue(PurchaseFlow.blocksNewPurchase(restarted.recoveryState(owner)))
        assertFalse(restarted.restart())
    }

    @Test fun `cancelled and expired reconciling orders keep original status retry until later paid consumed`() {
        // Native maps provider canceled to the established failed payment status.
        for (status in listOf("failed", "expired")) {
            val disk = Disk()
            val (attempts, _) = prepared(disk)
            attempts.savePayment(owner, paymentEvent("pending", "reserved").toString())
            val closing = attempts.savePayment(owner, paymentEvent(status, "reconciling").toString())
            val restarted = PurchaseAttempts(disk) { error("no new Q/K during reconciliation") }
            val state = restarted.recoveryState(owner)!!
            assertEquals("unresolved", closing.order!!.outcome)
            assertEquals("reconciling", state.payment!!.referralDiscountState)
            assertEquals(PurchaseFlow.AWAITING_PAYMENT, state.phase)
            assertFalse(PurchaseFlow.terminalPayment(state.payment))
            assertTrue(PurchaseFlow.blocksNewPurchase(state))
            assertEquals(PurchaseOperation.PaymentGet(state.payment.paymentId), restarted.recoveryOperation(owner))
            assertFalse(restarted.restart())
            rejected { restarted.beginQuote(selection()) }
            val paid = restarted.savePayment(owner, paymentEvent().toString())
            assertEquals("consumed", paid.order!!.payment!!.referralDiscountState)
            assertEquals(pricing, paid.order.payment!!.creditedProduct!!.pricing)
            val afterPaid = PurchaseAttempts(disk).recoveryState(owner)!!
            assertEquals(PurchaseFlow.AWAITING_CONFIRMATION, afterPaid.phase)
            assertFalse(afterPaid.freshMeConfirmed)
            assertTrue(PurchaseFlow.blocksNewPurchase(afterPaid))
        }
    }

    @Test fun `missing optional discount state cannot release priced invoice or erase known reservation`() {
        val disk = Disk()
        val (attempts, _) = prepared(disk)
        attempts.savePayment(owner, paymentEvent("pending", "reserved").toString())
        val before = disk.read()
        rejected { attempts.savePayment(owner, paymentEvent("expired", null).toString()) }
        assertEquals(before, disk.read())
        assertFalse(PurchaseAttempts(disk).restart())
        val missing = (PaymentsContract.parse(paymentEvent("expired", null)) as PaymentsEvent.Payment).payment
        assertFalse(PurchaseFlow.terminalPayment(missing))
        assertTrue(PurchaseFlow.blocksNewPurchase(PurchaseFlow.paymentResult(null, missing)))
        val otherDisk = Disk()
        val (other, _) = prepared(otherDisk)
        assertEquals("unresolved", other.savePayment(owner, paymentEvent("expired", null).toString()).order!!.outcome)
        assertFalse(PurchaseAttempts(otherDisk).restart())
    }

    @Test fun `changed pricing product credit or consumed state cannot replace original commercial receipt`() {
        val disk = Disk()
        val (attempts, _) = prepared(disk)
        attempts.savePayment(owner, paymentEvent().toString())
        val before = disk.read()
        val changedPricing = pricing.copy(baseAmountMinor = 30_168, payableAmountMinor = 20_168)
        val changed = paymentEvent().apply {
            getJSONObject("product").put("extra_amount_minor", 10_168)
            put("pricing", PaymentPricingCodec.encode(changedPricing))
            getJSONObject("credited_product").put("pricing", PaymentPricingCodec.encode(changedPricing))
        }
        rejected { attempts.savePayment(owner, changed.toString()) }
        rejected { attempts.savePayment(owner, paymentEvent("expired", "reconciling").toString()) }
        rejected { attempts.savePayment(owner, paymentEvent().apply {
            getJSONObject("credited_product").put("pricing", PaymentPricingCodec.encode(changedPricing))
        }.toString()) }
        assertEquals(before, disk.read())
        assertEquals(pricing, PurchaseAttempts(disk).current()!!.order!!.payment!!.pricing)
    }

    @Test fun `exact no order proofs commit atomically preserve history and allow only explicit new quote`() {
        for (reason in listOf("referral_discount_reserved", "referral_quote_changed")) {
            val disk = Disk()
            val (attempts, flight) = prepared(disk)
            val original = disk.read()
            disk.beforeCommit = {
                assertEquals(original, disk.read())
                assertTrue(PurchaseFlow.blocksNewPurchase(attempts.recoveryState(owner)))
            }
            val resolved = resolve(attempts, flight, proof(flight, reason))
            assertEquals("referral_no_order", resolved.order!!.outcome)
            assertFalse(resolved.unresolved)
            assertEquals(flight.paymentIntent!!.paymentKey, resolved.paymentKey)
            assertEquals(pricing, resolved.frozenQuote!!.pricing)
            assertEquals(1, disk.commits)
            assertEquals("preserved", disk.state.getString("other_namespace"))
            val history = disk.state.getJSONObject("purchase_resolution_history").getJSONObject(resolved.attemptId)
            assertEquals(original, history.getString("original"))
            val restarted = PurchaseAttempts(disk)
            val state = restarted.recoveryState(owner)!!
            assertEquals(PurchaseFlow.NO_ORDER, state.phase)
            assertEquals(reason, state.noOrderReason)
            assertFalse(PurchaseFlow.blocksNewPurchase(state))
            assertNull(state.payment)
            assertNull(state.createAck)
            assertNull(restarted.recoveryState("foreign-account"))
            assertEquals(resolved, resolve(restarted, flight, proof(flight, reason)))
            assertEquals(1, disk.commits)
            rejected { restarted.retryCreate(owner) }
            disk.beforeCommit = null
            assertNotEquals(resolved.paymentKey, restarted.beginQuote(selection()).paymentKey)
            val newer = disk.read()
            rejected { resolve(restarted, flight, proof(flight, reason)) }
            assertEquals(newer, disk.read())
            assertEquals(original, history.getString("original"))
        }
    }

    @Test fun `no order refuses GET bare errors foreign Q K account installation flight and malformed proof`() {
        val disk = Disk()
        val (attempts, flight) = prepared(disk)
        val before = disk.state.toString()
        val invalid = listOf(
            proof(flight).put("type", PaymentsContract.TYPE_PAYMENT_GET_RESULT),
            proof(flight).apply { remove("create_resolution") },
            proof(flight).put("http_status", "409"), proof(flight).put("retryable", true),
            proof(flight).put("request_id", "invalid"), proof(flight).put("code", "QUOTE_EXPIRED"),
            proof(flight).apply { getJSONObject("create_resolution").put("quote_id", "00000000-0000-4000-8000-000000000001") },
            proof(flight).apply { getJSONObject("create_resolution").put("request_idempotency_key", "another-idempotency-key") },
            proof(flight).apply { getJSONObject("create_resolution").put("reason", "unknown") },
            proof(flight).put("attempt_id", "foreign-native-attempt"),
        )
        invalid.forEach { rejected { resolve(attempts, flight, it) } }
        for (badFlight in listOf(flight.copy(kind = PurchaseFlightKind.PAYMENT_GET), flight.copy(paymentIntent = null),
            flight.copy(installationId = "b".repeat(64)), flight.copy(idempotencyKey = "another-idempotency-key"),
            flight.copy(quoteId = "foreign-quote"), flight.copy(attempt = "foreign-native-attempt"))) {
            rejected { resolve(attempts, badFlight) }
        }
        for (account in listOf(null, "foreign-account")) {
            rejected { attempts.resolveNoOrder(flight, nativeAttempt, account, installation, proof(flight).toString()) }
        }
        assertEquals(before, disk.state.toString())
        assertTrue(attempts.current()!!.unresolved)
    }

    @Test fun `failed commit or intervening disk writer preserves original unknown and CAS fence`() {
        val disk = Disk()
        val (attempts, flight) = prepared(disk)
        val before = disk.state.toString()
        disk.failCommit = true
        rejected { resolve(attempts, flight) }
        assertEquals(before, disk.state.toString())
        assertEquals(flight.paymentIntent, PurchaseAttempts(disk).retryCreate(owner))
        assertEquals(0, disk.commits)
        disk.failCommit = false
        val intervening = flight.paymentIntent!!.copy(order = SavedPurchaseOrder(owner,
            paymentEvent("pending", "reserved").toString()))
        disk.beforeCommit = { disk.write(PurchaseAttemptCodec.encode(intervening)) }
        rejected { resolve(attempts, flight) }
        assertEquals(intervening, attempts.current())
        assertFalse(disk.state.has("purchase_resolution_history"))
        assertFalse(PurchaseAttempts(disk).restart())
    }
}

package xyz.terlimo.test

import org.json.JSONObject

/** Service result boundary: displayed history never authorizes or vetoes the current intent. */
internal class PurchaseResultAcceptance(
    private val attempts: PurchaseAttempts,
    private val creates: PaymentCreateTracker,
) {
    fun payment(current: PurchaseState?, flight: PurchaseFlight, activeAttempt: String,
        accountRef: String?, installationId: String, event: JSONObject): PurchaseState {
        val parsed = PaymentsContract.parse(event) as PaymentsEvent.Payment
        check(flight.kind.resultType == parsed.type && flight.attempt == activeAttempt &&
            event.getString("attempt_id") == activeAttempt)
        val isCreate = parsed.type == PaymentsContract.TYPE_PAYMENT_CREATE_RESULT
        val captured = if (isCreate) {
            val intent = flight.paymentIntent ?: error("PURCHASE_CREATE_UNCORRELATED")
            check(flight.kind == PurchaseFlightKind.PAYMENT && flight.installationId == installationId &&
                installationId.matches(Regex("^[0-9a-f]{64}$")) && accountRef != null &&
                intent.order?.accountRef == accountRef && intent.unresolved &&
                intent.order.paymentEvent == null && intent.frozenQuote != null &&
                flight.quoteId == intent.quoteId && flight.idempotencyKey == intent.paymentKey)
            intent
        } else {
            check(attempts.current()?.order?.payment?.paymentId == parsed.payment.paymentId) {
                "PURCHASE_RECEIPT_MISMATCH"
            }
            null
        }
        // The same journal read validates the captured preimage before the receipt is saved.
        val saved = attempts.savePayment(accountRef, event.toString(), captured)
        // No projection or acknowledgement is produced before durable acceptance returns.
        val base = if (isCreate) (current ?: PurchaseState()).copy(
            payment = null, createAck = null, quote = saved.frozenQuote,
            quoteBinding = PurchaseQuoteBinding(checkNotNull(saved.frozenQuote), checkNotNull(saved.selection)),
            ownerAccountRef = saved.order?.accountRef, freshMeConfirmed = false,
            confirmationPlansLoaded = false,
        ) else current
        val ack = creates.onCreateResult(parsed.type, parsed.payment)
        val terminal = parsed.payment.paymentStatus == "paid" || PurchaseFlow.terminalPayment(parsed.payment)
        if (terminal) creates.clear()
        val result = when {
            terminal -> PurchaseFlow.paymentResult(base, parsed.payment).copy(createAck = null)
            ack != null -> PurchaseFlow.paymentCreateResult(base, parsed.payment, ack)
            else -> PurchaseFlow.paymentGetResult(base, parsed.payment)
        }
        return result.copy(ownerAccountRef = saved.order?.accountRef,
            recovery = if (saved.unresolved) "known_payment" else null)
    }

    fun noOrder(current: PurchaseState?, flight: PurchaseFlight, activeAttempt: String,
        accountRef: String?, installationId: String, event: JSONObject): PurchaseState {
        // resolveNoOrder checks captured/disk no-payment, Q/K/owner/install and atomic CAS.
        // A historical displayed receipt is unrelated to that authoritative preimage.
        val saved = attempts.resolveNoOrder(flight, activeAttempt, accountRef, installationId, event.toString())
        creates.clear()
        val proof = PaymentsContract.parse(event) as PaymentsEvent.Failure
        return if (proof.expiredNoOrder)
            PurchaseFlow.expiredNoOrderState(current, saved.order!!.accountRef)
        else PurchaseFlow.noOrderState(current, saved.order!!.accountRef, checkNotNull(proof.createResolution).reason)
    }
}

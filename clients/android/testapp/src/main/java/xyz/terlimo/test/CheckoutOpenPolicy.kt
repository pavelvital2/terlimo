package xyz.terlimo.test

import android.os.Bundle

/**
 * §3.2B correction: the provider browser may open ONLY as the consequence of an explicit
 * user action — the «Оплатить» tap that sends `purchase_pay` or an explicit
 * «Продолжить оплату»/«Повторить оплату» tap for an already-created payment. An ordinary
 * status refresh, polling render or Activity recreation never opens it.
 *
 * The explicit «Оплатить» tap arms a live (never persisted) marker with the exact sent quote
 * id. [autoOpenAfterPay] may consume it exactly once and only through a correlated
 * [PaymentCreateAck] whose `quoteId` equals the armed key and whose `seq` was not consumed
 * before: the payment opened here comes from the create result of this very send, never from
 * any payment kept in ViewState under a replaced quote (a stale order cannot steal the
 * open). A null ack keeps the marker armed for the awaited result; a terminal payment status
 * (`paid`, `failed`, `expired`, `refunded`, `disputed`) never opens, and the opened payment
 * id suppresses only the automatic repeat, never a deliberate continue. Only a successful
 * `startActivity` records [onOpened]; a failed launch records [onOpenFailed] so the payment
 * stays retryable through the explicit continue action and the error stays visible.
 * Returning from the browser never marks anything paid here: confirmation comes only from a
 * fresh accepted server /me (PurchaseFlow).
 */
internal data class CheckoutOpenSavedState(
    val openedPaymentId: String?,
    val openError: String?,
    val lastConsumedAckSeq: Long? = null,
)

internal class CheckoutOpenPolicy {
    private var awaitingKey: String? = null
    private var openedPaymentId: String? = null

    /** Highest create generation already consumed; persisted so recreation never re-consumes it. */
    private var lastConsumedAckSeq: Long? = null

    /** Visible error of the last failed launch; null when nothing failed. */
    var openError: String? = null
        private set

    /**
     * Explicit «Оплатить» tap: arms exactly one live auto-open for the current attempt key.
     * A different key replaces (never merges with) the previous marker; an empty key arms
     * nothing.
     */
    fun onPayRequested(attemptKey: String) {
        awaitingKey = attemptKey.ifEmpty { null }
        openError = null
    }

    /** Payment-create/attempt failure: the armed auto-open must not fire for a later payment. */
    fun onPayFailed() = clearAwaiting()

    /** Drops the live marker (attempt replaced or errored) without touching opened id/error. */
    fun clearAwaiting() {
        awaitingKey = null
    }

    /**
     * Live consequence of the explicit pay tap only, and only through the correlated create
     * result of the exact sent quote: an ack with a different quote id (another attempt)
     * returns null without consuming the marker; a null ack or an already-consumed generation
     * also returns null, so an ordinary render, a status refresh and a stale acknowledgement
     * can never steal or spend the open of the awaited create. The matching ack is consumed
     * exactly once before the caller launches (the marker is dropped even when the status or
     * URL then refuses the open, so a failed attempt surfaces an error instead of looping).
     * Terminal statuses return null and an already successfully opened payment is not
     * auto-opened again.
     */
    fun autoOpenAfterPay(ack: PaymentCreateAck?): CheckoutOpen? {
        val armed = awaitingKey ?: return null
        val current = ack ?: return null
        if (current.quoteId != armed) return null
        if (lastConsumedAckSeq?.let { current.seq <= it } == true) return null
        awaitingKey = null
        lastConsumedAckSeq = current.seq
        if (current.payment.paymentId == openedPaymentId) return null
        return openFor(current.payment)
    }

    /**
     * Explicit «Продолжить оплату»/«Повторить оплату» for an existing payment: the saved
     * order/idempotency is reused, no new invoice or payment_create is issued. A deliberate
     * continue reopens the same saved payment even when it was opened before; it only refuses
     * terminal statuses and an unreadable URL.
     */
    fun onContinueRequested(payment: PaymentStatusView?): CheckoutOpen? {
        val current = payment ?: return null
        openError = null
        return openFor(current)
    }

    /** The explicit continue/retry action is offered for a non-terminal payment with a valid link. */
    fun canContinue(payment: PaymentStatusView?): Boolean {
        val current = payment ?: return false
        if (current.paymentId.isEmpty()) return false
        if (!openable(current)) return false
        return CheckoutRedirect.providerUrl(current.checkoutReference) != null
    }

    /** Only a successful `startActivity` records this; it also clears the visible error. */
    fun onOpened(paymentId: String) {
        openedPaymentId = paymentId
        openError = null
    }

    /** A failed launch stays retryable: the id is not marked opened, the error is visible. */
    fun onOpenFailed(error: String) {
        openError = error
    }

    private fun openFor(payment: PaymentStatusView): CheckoutOpen? {
        if (payment.paymentId.isEmpty() || !openable(payment)) return null
        val url = CheckoutRedirect.providerUrl(payment.checkoutReference)
        if (url == null) {
            openError = PaymentsText.CHECKOUT_INVALID_LINK_TEXT
            return null
        }
        return CheckoutOpen(payment.paymentId, url)
    }

    /** Only the wire statuses of a live checkout may open; every terminal status never does. */
    private fun openable(payment: PaymentStatusView): Boolean =
        payment.paymentStatus == STATUS_CREATED || payment.paymentStatus == STATUS_PENDING

    fun savedState(): CheckoutOpenSavedState =
        CheckoutOpenSavedState(openedPaymentId, openError, lastConsumedAckSeq)

    /**
     * State restored after recreation: the opened id, the visible error and the consumed
     * create generation survive so the same payment/ack is not auto-opened or re-consumed
     * again, while the live attempt marker is deliberately never restored — a recreated
     * Activity can never auto-open anything (only an explicit action can).
     */
    fun restore(saved: CheckoutOpenSavedState?) {
        openedPaymentId = saved?.openedPaymentId
        openError = saved?.openError
        lastConsumedAckSeq = saved?.lastConsumedAckSeq
        awaitingKey = null
    }

    fun saveTo(outState: Bundle) {
        val saved = savedState()
        saved.openedPaymentId?.let { outState.putString(STATE_OPENED_PAYMENT_ID, it) }
        saved.openError?.let { outState.putString(STATE_OPEN_ERROR, it) }
        saved.lastConsumedAckSeq?.let { outState.putLong(STATE_LAST_CONSUMED_ACK_SEQ, it) }
    }

    fun restoreFrom(savedState: Bundle?) {
        restore(savedState?.let {
            CheckoutOpenSavedState(
                openedPaymentId = it.getString(STATE_OPENED_PAYMENT_ID),
                openError = it.getString(STATE_OPEN_ERROR),
                lastConsumedAckSeq = if (it.containsKey(STATE_LAST_CONSUMED_ACK_SEQ))
                    it.getLong(STATE_LAST_CONSUMED_ACK_SEQ) else null,
            )
        })
    }

    companion object {
        /** The only wire payment statuses that may open the provider browser (PaymentsContract). */
        const val STATUS_CREATED = "created"
        const val STATUS_PENDING = "pending"

        const val STATE_OPENED_PAYMENT_ID = "checkout_opened_payment_id"
        const val STATE_OPEN_ERROR = "checkout_open_error"
        const val STATE_LAST_CONSUMED_ACK_SEQ = "checkout_last_consumed_ack_seq"
    }
}

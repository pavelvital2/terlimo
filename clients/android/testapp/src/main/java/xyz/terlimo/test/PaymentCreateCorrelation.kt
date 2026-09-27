package xyz.terlimo.test

/**
 * Correlated acknowledgement of one explicitly sent `payment_create`. It is built ONLY from a
 * local `payment_create_result` that answered the recorded send of [quoteId]; a rendered
 * payment of [CheckoutOpenPolicy] can therefore never be an old order kept in ViewState under
 * a replaced quote. `seq` is the monotonic local create generation of that send: the policy
 * consumes each generation at most once.
 */
internal data class PaymentCreateAck(
    val quoteId: String,
    val seq: Long,
    val payment: PaymentStatusView,
)

/**
 * Host-local correlation of the single explicitly sent `payment_create` with its result.
 * The wire carries no quote id on `payment_create_result`/`payment_get_result`
 * (PaymentStatusView has none), so pairing is local: the send records the exact quote id and
 * a strictly increasing generation, and only the matching `payment_create_result` may build
 * an acknowledgement. A get result never does, an unsolicited create result never does, and
 * a second result for an already-consumed send never does. [clear] drops the send record on
 * every path that abandons the attempt (create failure, restart/replacement, QUOTE_EXPIRED,
 * attempt teardown), so a later unrelated payment cannot inherit the correlation.
 *
 * The generation is seeded from the wall clock and then strictly increasing: a consumed
 * generation persisted by the Activity before a process restart can never equal the next
 * generation of a fresh tracker, so an explicit tap after recreation still opens exactly once.
 */
internal class PaymentCreateTracker(seed: Long = System.currentTimeMillis()) {
    private var sentQuoteId: String? = null
    private var seq: Long = seed

    /** One explicit `payment_create` was sent for [quoteId]: record it with a new generation. */
    @Synchronized
    fun onSent(quoteId: String) {
        sentQuoteId = quoteId
        seq += 1
    }

    /**
     * The parsed result event: an ack only for the `payment_create_result` of the recorded
     * send; null for a `payment_get_result`, for an unsolicited create result and for any
     * duplicate result of a send whose ack was already built.
     */
    @Synchronized
    fun onCreateResult(type: String, payment: PaymentStatusView): PaymentCreateAck? {
        if (type != PaymentsContract.TYPE_PAYMENT_CREATE_RESULT) return null
        val quoteId = sentQuoteId ?: return null
        sentQuoteId = null
        return PaymentCreateAck(quoteId = quoteId, seq = seq, payment = payment)
    }

    /** Attempt abandoned: no pending create result may be correlated afterwards. */
    @Synchronized
    fun clear() {
        sentQuoteId = null
    }
}

package xyz.terlimo.test

/**
 * The purchase operations that may occupy the single native purchase stream. Each kind
 * releases the holder through exactly its own parsed result/failure type.
 */
internal enum class PurchaseFlightKind(val resultType: String) {
    PLANS(PaymentsContract.TYPE_PLANS_LIST_RESULT),
    QUOTE(PaymentsContract.TYPE_QUOTE_CREATE_RESULT),
    PAYMENT(PaymentsContract.TYPE_PAYMENT_CREATE_RESULT),
    PAYMENT_GET(PaymentsContract.TYPE_PAYMENT_GET_RESULT),
}

/**
 * One actually sent purchase operation on the native event stream: what was sent, for which
 * attempt, with which durable idempotency key. It is the transport-attempt identity the host
 * owns; the frozen wire carries no response identity, so pairing a response to a request is
 * only meaningful while at most one request is outstanding.
 */
internal data class PurchaseFlight(
    val kind: PurchaseFlightKind,
    val attempt: String,
    val quoteId: String? = null,
    val idempotencyKey: String? = null,
    val paymentIntent: PurchaseAttempt? = null,
    val installationId: String? = null,
)

/**
 * Host-owned single-flight gate for purchase requests. There is exactly one native purchase
 * event stream per active attempt, and the frozen server contract echoes no request identity
 * on `payment_create_result`/`payment_get_result`; therefore at most one Quote/Payment request
 * may be outstanding, so a late result can never be correlated with a later send under a
 * different quote (the reported defect).
 *
 * The gate is captured BEFORE any durable key is rotated (see SessionService.sendPurchaseOperation)
 * so a rejected request starts no new attempt and reuses no new key. It is released only by the
 * matching parsed result/failure of the operation that holds it (never by a plain selection
 * change or a cleared ViewState), and unconditionally by the transport teardown that fences the
 * attempt. A GET result can never release a create holder.
 *
 * Ownership is the capture's IDENTITY (reference), never a value rebuild: `attachKey` only adds
 * metadata to the same owner, so a preparation error after the key was attached still releases
 * the exact capture, while a stale capture can never free a newer owner.
 */
internal class PurchaseSingleFlight {
    /** The immutable capture identity holding the stream; null when free. */
    private var owner: PurchaseFlight? = null
    /** Trace metadata of the current owner (kept apart so identity is never rebuilt). */
    private var idempotencyKey: String? = null
    private var paymentIntent: PurchaseAttempt? = null
    private var installationId: String? = null

    @Synchronized fun holder(): PurchaseFlight? = owner?.copy(idempotencyKey = idempotencyKey, paymentIntent = paymentIntent, installationId = installationId)

    /** True while a purchase request is outstanding; the UI shows waiting and disables actions. */
    @Synchronized fun busy(): Boolean = owner != null

    /**
     * Capture the stream for one operation about to be sent. False when another purchase
     * request is still outstanding: the caller must not rotate keys or send anything.
     */
    @Synchronized fun acquire(flight: PurchaseFlight): Boolean {
        if (owner != null) return false
        owner = flight
        idempotencyKey = flight.idempotencyKey
        paymentIntent = null
        installationId = null
        return true
    }

    /** Records the durable idempotency key of the captured send; identity is unchanged. */
    @Synchronized fun attachKey(flight: PurchaseFlight, key: String?) {
        if (owner === flight) idempotencyKey = key
    }

    @Synchronized fun attachCreate(flight: PurchaseFlight, intent: PurchaseAttempt, installation: String) {
        if (owner === flight) {
            paymentIntent = intent
            installationId = installation
        }
    }

    /** A changed verified account permanently fences this outstanding no-create proof. */
    @Synchronized fun invalidateCreateProof() { paymentIntent = null; installationId = null }

    /**
     * Releases the holder only for the matching parsed result/failure type of the operation
     * that occupies the stream; a different (e.g. GET) result leaves it occupied.
     */
    @Synchronized fun releaseOn(resultType: String): Boolean {
        val current = owner ?: return false
        if (current.kind.resultType != resultType) return false
        owner = null
        idempotencyKey = null
        paymentIntent = null
        installationId = null
        return true
    }

    /** A local refusal of the exact captured send (identity): the capture is dropped at once. */
    @Synchronized fun release(flight: PurchaseFlight) {
        if (owner === flight) {
            owner = null
            idempotencyKey = null
            paymentIntent = null
            installationId = null
        }
    }

    /** Transport teardown / attempt stop: the old native stream is fenced, drop the holder. */
    @Synchronized fun reset() {
        owner = null
        idempotencyKey = null
        paymentIntent = null
        installationId = null
    }
}

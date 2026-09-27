package xyz.terlimo.test

import org.json.JSONObject
import java.util.UUID

/** Encrypted AtomicFile-backed purchase attempt memory; injected for pure tests. */
internal interface PurchaseAttemptStore {
    fun read(): String?
    fun write(encoded: String)
}

/**
 * One host-owned purchase attempt identity with its two idempotency keys. The record is
 * persisted BEFORE the first send of each operation, so an HTTP retry, a timeout or an
 * ambiguous response — including one across a process restart — reuses the exact same
 * key bytes. A deliberate new attempt (restart / changed plan / changed quote) gets a
 * new attempt identity and a new key pair.
 */
internal data class PurchaseAttempt(
    val attemptId: String,
    val planId: String?,
    val durationCode: String?,
    val method: String?,
    val quoteId: String?,
    val quoteKey: String,
    val paymentKey: String,
)

internal object PurchaseAttemptCodec {
    private val FIELDS = setOf(
        "attempt_id", "plan_id", "duration_code", "method", "quote_id", "quote_key", "payment_key",
    )
    private val ATTEMPT_ID = Regex("^[A-Za-z0-9._~-]{1,64}$")

    fun encode(attempt: PurchaseAttempt): String = JSONObject()
        .put("attempt_id", attempt.attemptId)
        .put("plan_id", attempt.planId ?: JSONObject.NULL)
        .put("duration_code", attempt.durationCode ?: JSONObject.NULL)
        .put("method", attempt.method ?: JSONObject.NULL)
        .put("quote_id", attempt.quoteId ?: JSONObject.NULL)
        .put("quote_key", attempt.quoteKey)
        .put("payment_key", attempt.paymentKey)
        .toString()

    /** Strict decode; a missing, cleared or malformed record is read as "no attempt". */
    fun decode(raw: String?): PurchaseAttempt? {
        if (raw.isNullOrEmpty()) return null
        return try {
            val json = JSONObject(raw)
            if (json.length() == 0) return null
            check(json.keys().asSequence().toSet() == FIELDS) { "PURCHASE_STATE_INVALID" }
            val attemptId = json.getString("attempt_id")
            check(ATTEMPT_ID.matches(attemptId)) { "PURCHASE_STATE_INVALID" }
            val planId = bounded(json, "plan_id", 128)
            val durationCode = bounded(json, "duration_code", 64)?.also {
                check(it in PaymentsContract.DURATION_CODES) { "PURCHASE_STATE_INVALID" }
            }
            val method = bounded(json, "method", 64)?.also {
                check(it in PaymentsContract.METHODS) { "PURCHASE_STATE_INVALID" }
            }
            val quoteId = bounded(json, "quote_id", 128)
            val quoteKey = json.getString("quote_key")
            val paymentKey = json.getString("payment_key")
            check(usableKey(quoteKey) && usableKey(paymentKey)) { "PURCHASE_STATE_INVALID" }
            PurchaseAttempt(attemptId, planId, durationCode, method, quoteId, quoteKey, paymentKey)
        } catch (_: Exception) {
            null
        }
    }

    /** The host-owned key shape native forwards verbatim: 16..128 bytes, no header injection. */
    fun usableKey(key: String): Boolean =
        key.length in 16..128 && !key.contains('\r') && !key.contains('\n')

    private fun bounded(source: JSONObject, key: String, maxLength: Int): String? {
        if (!source.has(key) || source.isNull(key)) return null
        val value = source.get(key)
        check(value is String && value.isNotEmpty() && value.length <= maxLength) {
            "PURCHASE_STATE_INVALID"
        }
        return value
    }
}

/**
 * Durable idempotency policy of the purchase flow. Quote and payment keys are decided
 * here, persisted before the caller may send anything, and reused for every retry of the
 * same business operation. A changed plan/method or a changed quote is a new attempt.
 */
internal class PurchaseAttempts(
    private val store: PurchaseAttemptStore,
    private val newKey: () -> String = { "terlimo-" + UUID.randomUUID() },
) {
    @Synchronized
    fun current(): PurchaseAttempt? = PurchaseAttemptCodec.decode(store.read())

    /**
     * Returns the attempt to use for one quote_create. The same plan+duration+method whose
     * quote has not been bound yet reuses its keys, so a retry after a timeout or an
     * ambiguous response sends the identical Idempotency-Key.
     */
    @Synchronized
    fun beginQuote(planId: String, durationCode: String, method: String): PurchaseAttempt {
        val existing = current()
        if (existing != null && existing.planId == planId && existing.durationCode == durationCode &&
            existing.method == method && existing.quoteId == null) {
            return existing
        }
        val next = PurchaseAttempt(
            attemptId = UUID.randomUUID().toString(),
            planId = planId, durationCode = durationCode, method = method, quoteId = null,
            quoteKey = generate(), paymentKey = generate(),
        )
        persist(next)
        return next
    }

    /** Binds the accepted quote id to the current attempt; null when no attempt is stored. */
    @Synchronized
    fun bindQuote(quoteId: String): PurchaseAttempt? {
        val existing = current() ?: return null
        if (existing.quoteId == quoteId) return existing
        val next = existing.copy(quoteId = quoteId)
        persist(next)
        return next
    }

    /**
     * Returns the attempt to use for one payment_create of [quoteId]. Retrying the same
     * quote reuses the persisted payment key; an unknown/changed quote starts a fresh
     * identity (and can therefore never reuse a key bound to other data).
     */
    @Synchronized
    fun beginPayment(quoteId: String): PurchaseAttempt {
        val existing = current()
        if (existing != null && existing.quoteId == quoteId) return existing
        val next = PurchaseAttempt(
            attemptId = UUID.randomUUID().toString(),
            planId = existing?.planId, durationCode = existing?.durationCode, method = existing?.method,
            quoteId = quoteId, quoteKey = generate(), paymentKey = generate(),
        )
        persist(next)
        return next
    }

    /** A deliberate new attempt drops the stored identity, so the next begin* makes new keys. */
    @Synchronized
    fun restart() {
        store.write("{}")
    }

    internal fun generate(): String {
        val key = newKey()
        require(PurchaseAttemptCodec.usableKey(key)) { "PURCHASE_KEY_INVALID" }
        return key
    }

    private fun persist(attempt: PurchaseAttempt) {
        store.write(PurchaseAttemptCodec.encode(attempt))
    }
}

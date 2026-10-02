package xyz.terlimo.test

import org.json.JSONArray
import org.json.JSONObject
import java.util.UUID

/** Encrypted AtomicFile-backed purchase attempt memory; injected for pure tests. */
internal interface PurchaseAttemptStore {
    fun read(): String?
    fun write(encoded: String)
    /** Must compare the current bytes and commit intent/history/result in one atomic write. */
    fun resolveNoOrder(expected: String, resolved: String, installationId: String) {
        error("PURCHASE_ATOMIC_RESOLUTION_UNAVAILABLE")
    }
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
    val selection: PurchaseSelection? = null,
    val formatVersion: Int = 2,
    val quoteEvent: String? = null,
    val order: SavedPurchaseOrder? = null,
) {
    val frozenQuote: PaymentQuote? get() = quoteEvent?.let {
        (PaymentsContract.parse(JSONObject(it)) as PaymentsEvent.Quote).quote
    }
    val legacyUncertain: Boolean get() = formatVersion < 2 && quoteId != null
    val unresolved: Boolean get() = legacyUncertain || order?.outcome == "unresolved"
}

/** Stored inside the same encrypted atomic record as Q/K and the accepted quote. */
internal data class SavedPurchaseOrder(
    val accountRef: String,
    val paymentEvent: String? = null,
    val outcome: String = "unresolved",
    val noCreateEvent: String? = null,
    val noCreateInstallationId: String? = null,
) {
    val payment: PaymentStatusView? get() = paymentEvent?.let {
        (PaymentsContract.parse(JSONObject(it)) as PaymentsEvent.Payment).payment
    }
}

internal object PurchaseAttemptCodec {
    private val FIELDS = setOf(
        "attempt_id", "plan_id", "duration_code", "method", "quote_id", "quote_key", "payment_key",
    )
    private val SELECTION_FIELDS = setOf("plan_id", "duration_code", "method", "product_kind",
        "target_entitlement_id", "target_valid_until", "renew_extra_slot_ids")
    private val ATTEMPT_ID = Regex("^[A-Za-z0-9._~-]{1,64}$")

    fun encode(attempt: PurchaseAttempt): String {
        val json = JSONObject()
            .put("attempt_id", attempt.attemptId)
            .put("plan_id", attempt.planId ?: JSONObject.NULL)
            .put("duration_code", attempt.durationCode ?: JSONObject.NULL)
            .put("method", attempt.method ?: JSONObject.NULL)
            .put("quote_id", attempt.quoteId ?: JSONObject.NULL)
            .put("quote_key", attempt.quoteKey)
            .put("payment_key", attempt.paymentKey)
        attempt.selection?.let { selected -> json.put("selection", JSONObject()
            .put("plan_id", selected.planId).put("duration_code", selected.durationCode)
            .put("method", selected.method).put("product_kind", selected.productKind ?: JSONObject.NULL)
            .put("target_entitlement_id", selected.targetEntitlementId ?: JSONObject.NULL)
            .put("target_valid_until", selected.targetValidUntil ?: JSONObject.NULL)
            .put("renew_extra_slot_ids", JSONArray(selected.renewExtraSlotIds))) }
        if (attempt.formatVersion == 2) {
            json.put("format_version", 2)
                .put("quote_event", attempt.quoteEvent?.let(::JSONObject) ?: JSONObject.NULL)
                .put("order", attempt.order?.let {
                    JSONObject().put("account_ref", it.accountRef).put("outcome", it.outcome)
                        .put("payment_event", it.paymentEvent?.let(::JSONObject) ?: JSONObject.NULL)
                        .also { json -> it.noCreateEvent?.let { event -> json.put("no_create", JSONObject()
                            .put("installation_id", it.noCreateInstallationId)
                            .put("event", JSONObject(event))) } }
                } ?: JSONObject.NULL)
        }
        return json.toString()
    }

    /** Strict decode; a missing, cleared or malformed record is read as "no attempt". */
    fun decode(raw: String?): PurchaseAttempt? {
        if (raw.isNullOrEmpty()) return null
        return try {
            val json = JSONObject(raw)
            if (json.length() == 0) return null
            val version = if (json.has("format_version")) json.get("format_version") else 1
            check(version is Int && version in 1..2) { "PURCHASE_STATE_INVALID" }
            val expected = FIELDS + (if (json.has("selection")) setOf("selection") else emptySet()) +
                (if (version == 2) setOf("format_version", "quote_event", "order") else emptySet())
            check(json.keys().asSequence().toSet() == expected) { "PURCHASE_STATE_INVALID" }
            val attemptId = json.getString("attempt_id")
            check(ATTEMPT_ID.matches(attemptId)) { "PURCHASE_STATE_INVALID" }
            val planId = bounded(json, "plan_id", 128)
            val durationCode = bounded(json, "duration_code", 64)?.also {
                check(validDuration(it)) { "PURCHASE_STATE_INVALID" }
            }
            val method = bounded(json, "method", 64)?.also {
                check(it in PaymentsContract.METHODS) { "PURCHASE_STATE_INVALID" }
            }
            val quoteId = bounded(json, "quote_id", 128)
            val quoteKey = json.getString("quote_key")
            val paymentKey = json.getString("payment_key")
            check(usableKey(quoteKey) && usableKey(paymentKey)) { "PURCHASE_STATE_INVALID" }
            val selection = if (json.has("selection")) decodeSelection(json.getJSONObject("selection")) else null
            check(selection == null || (selection.planId == planId && selection.durationCode == durationCode &&
                selection.method == method)) { "PURCHASE_STATE_INVALID" }
            val quoteEvent = if (version == 2 && !json.isNull("quote_event"))
                json.getJSONObject("quote_event").toString() else null
            val quote = quoteEvent?.let { (PaymentsContract.parse(JSONObject(it)) as PaymentsEvent.Quote).quote }
            check(quote == null || quote.quoteId == quoteId) { "PURCHASE_STATE_INVALID" }
            val order = if (version == 2 && !json.isNull("order")) {
                val saved = json.getJSONObject("order")
                check(saved.keys().asSequence().toSet() == setOf("account_ref", "outcome", "payment_event") +
                    if (saved.has("no_create")) setOf("no_create") else emptySet())
                val account = bounded(saved, "account_ref", 256) ?: error("PURCHASE_STATE_INVALID")
                val outcome = saved.getString("outcome")
                check(outcome in setOf("unresolved", "terminal", "confirmed", "expired_no_order"))
                val paymentEvent = if (saved.isNull("payment_event")) null else saved.getJSONObject("payment_event").toString()
                val proof = if (saved.has("no_create")) saved.getJSONObject("no_create") else null
                check((outcome == "expired_no_order") == (proof != null))
                val noCreateEvent = proof?.let {
                    check(it.keys().asSequence().toSet() == setOf("installation_id", "event"))
                    check(it.getString("installation_id").matches(Regex("^[0-9a-f]{64}$")))
                    it.getJSONObject("event").toString().also { raw ->
                        check((PaymentsContract.parse(JSONObject(raw)) as? PaymentsEvent.Failure)?.expiredNoOrder == true)
                    }
                }
                val result = SavedPurchaseOrder(account, paymentEvent, outcome, noCreateEvent,
                    proof?.getString("installation_id"))
                check(proof == null || paymentEvent == null)
                val payment = result.payment
                check(quote != null && selection != null)
                check(outcome != "terminal" || payment?.let(PurchaseFlow::terminalPayment) == true)
                check(outcome != "confirmed" || (payment?.paymentStatus == "paid" && payment.creditState != "needs_review"))
                result
            } else null
            PurchaseAttempt(attemptId, planId, durationCode, method, quoteId, quoteKey, paymentKey,
                selection, version, quoteEvent, order)
        } catch (_: Exception) {
            null
        }
    }

    private fun validDuration(value: String): Boolean = value in PaymentsContract.DURATION_CODES ||
        (value.startsWith("until:") && LocalStamp.parseUtc(value.removePrefix("until:")) != null)

    private fun decodeSelection(json: JSONObject): PurchaseSelection {
        check(json.keys().asSequence().toSet() == SELECTION_FIELDS) { "PURCHASE_STATE_INVALID" }
        val planId = bounded(json, "plan_id", 128) ?: error("PURCHASE_STATE_INVALID")
        val duration = bounded(json, "duration_code", 64) ?: error("PURCHASE_STATE_INVALID")
        val method = bounded(json, "method", 64) ?: error("PURCHASE_STATE_INVALID")
        check(validDuration(duration) && method in PaymentsContract.METHODS) { "PURCHASE_STATE_INVALID" }
        val kind = bounded(json, "product_kind", 64)
        check(kind == null || kind in setOf("subscription", "device_addon")) { "PURCHASE_STATE_INVALID" }
        val target = bounded(json, "target_entitlement_id", 128)
        val targetEnd = bounded(json, "target_valid_until", 64)
        check(targetEnd == null || LocalStamp.parseUtc(targetEnd) != null) { "PURCHASE_STATE_INVALID" }
        val array = json.getJSONArray("renew_extra_slot_ids")
        val ids = (0 until array.length()).map { index ->
            val id = array.get(index)
            check(id is String && id.isNotEmpty() && id.length <= 128) { "PURCHASE_STATE_INVALID" }
            id as String
        }
        check(ids == ids.sorted() && ids.map { it.lowercase() }.distinct().size == ids.size &&
            (kind == "subscription" || ids.isEmpty())) {
            "PURCHASE_STATE_INVALID"
        }
        return PurchaseSelection(planId, duration, method, kind, target, targetEnd, ids)
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
    fun current(): PurchaseAttempt? {
        val raw = store.read()
        if (raw.isNullOrBlank() || raw.trim() == "{}") return null
        return PurchaseAttemptCodec.decode(raw) ?: error("PURCHASE_STATE_INVALID")
    }

    /**
     * Returns the attempt to use for one quote_create. The complete same selection whose
     * quote has not been bound yet reuses its keys, so a retry after a timeout or an
     * ambiguous response sends the identical Idempotency-Key.
     */
    @Synchronized
    fun beginQuote(planId: String, durationCode: String, method: String): PurchaseAttempt =
        beginQuote(PurchaseSelection(planId, durationCode, method))

    @Synchronized
    fun beginQuote(selection: PurchaseSelection): PurchaseAttempt {
        require(selection.renewExtraSlotIds.map { it.lowercase() }.distinct().size == selection.renewExtraSlotIds.size) {
            "PURCHASE_SELECTION_INVALID"
        }
        val selected = selection.copy(renewExtraSlotIds = selection.renewExtraSlotIds.sorted())
        val existing = current()
        check(existing?.unresolved != true) { "PURCHASE_RECOVERY_REQUIRED" }
        val existingSelection = existing?.selection ?: existing?.let {
            if (it.planId != null && it.durationCode != null && it.method != null)
                PurchaseSelection(it.planId, it.durationCode, it.method) else null
        }
        if (existing != null && existingSelection == selected && existing.quoteId == null) {
            if (existing.selection != null) return existing
            return existing.copy(selection = selected, formatVersion = 2).also { persist(it) }
        }
        val next = PurchaseAttempt(attemptId = UUID.randomUUID().toString(),
            planId = selected.planId, durationCode = selected.durationCode, method = selected.method,
            quoteId = null, quoteKey = generate(), paymentKey = generate(), selection = selected)
        persist(next)
        return next
    }

    /** Binds the accepted quote id to the current attempt; null when no attempt is stored. */
    @Synchronized
    fun bindQuote(quoteId: String, event: String? = null): PurchaseAttempt? {
        val existing = current() ?: return null
        check(!existing.unresolved) { "PURCHASE_RECOVERY_REQUIRED" }
        if (event != null) check((PaymentsContract.parse(JSONObject(event)) as PaymentsEvent.Quote).quote.quoteId == quoteId)
        if (existing.quoteId == quoteId && event == existing.quoteEvent) return existing
        val next = if (existing.quoteId == null || existing.quoteId == quoteId)
            existing.copy(quoteId = quoteId, quoteEvent = event, formatVersion = 2) else existing.copy(
            attemptId = UUID.randomUUID().toString(), quoteId = quoteId, quoteEvent = event, formatVersion = 2,
            quoteKey = generate(), paymentKey = generate())
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
        check(existing?.unresolved != true) { "PURCHASE_RECOVERY_REQUIRED" }
        if (existing != null && existing.quoteId == quoteId) return existing
        val next = PurchaseAttempt(
            attemptId = UUID.randomUUID().toString(),
            planId = existing?.planId, durationCode = existing?.durationCode, method = existing?.method,
            quoteId = quoteId, quoteKey = generate(), paymentKey = generate(), selection = existing?.selection,
        )
        persist(next)
        return next
    }

    /** A deliberate new attempt drops the stored identity, so the next begin* makes new keys. */
    @Synchronized
    fun restart(): Boolean {
        if (current()?.unresolved == true) return false
        store.write("{}")
        return true
    }

    /** Atomic intent journal, persisted before native may write the explicit Pay. */
    @Synchronized
    fun markCreate(accountRef: String, quote: PaymentQuote): PurchaseAttempt {
        check(accountRef.isNotBlank())
        val record = current() ?: error("PURCHASE_STATE_INVALID")
        check(!record.unresolved && record.selection != null && record.frozenQuote == quote)
        return record.copy(order = SavedPurchaseOrder(accountRef)).also { persist(it) }
    }

    /** Read-only recovery choice; no timeout/expiry path rotates Q/K. */
    @Synchronized
    fun recoveryOperation(accountRef: String?): PurchaseOperation {
        val record = current() ?: error("PURCHASE_STATE_INVALID")
        val order = record.order ?: error("PURCHASE_RECOVERY_UNAVAILABLE")
        check(accountRef != null && order.accountRef == accountRef) { "PURCHASE_ACCOUNT_MISMATCH" }
        val payment = order.payment
        return if (payment != null) PurchaseOperation.PaymentGet(payment.paymentId)
            else {
                check(record.unresolved) { "PURCHASE_RECOVERY_REQUIRED" }
                PurchaseOperation.Payment(record.quoteId!!, recovery = true)
            }
    }

    @Synchronized
    fun retryCreate(accountRef: String?): PurchaseAttempt {
        val record = current() ?: error("PURCHASE_STATE_INVALID")
        val order = record.order ?: error("PURCHASE_RECOVERY_REQUIRED")
        check(record.unresolved && order.accountRef == accountRef && accountRef != null &&
            order.payment == null && record.frozenQuote != null) { "PURCHASE_RECOVERY_REQUIRED" }
        return record
    }

    @Synchronized
    fun savePayment(accountRef: String?, event: String): PurchaseAttempt {
        val record = current() ?: error("PURCHASE_STATE_INVALID")
        val order = record.order ?: error("PURCHASE_STATE_INVALID")
        check(order.accountRef == accountRef && accountRef != null && order.noCreateEvent == null)
        val payment = (PaymentsContract.parse(JSONObject(event)) as PaymentsEvent.Payment).payment
        check(order.payment == null || order.payment?.paymentId == payment.paymentId)
        val outcome = when {
            PurchaseFlow.terminalPayment(payment) -> "terminal"
            order.outcome == "confirmed" && order.payment == payment -> "confirmed"
            else -> "unresolved"
        }
        return record.copy(order = order.copy(paymentEvent = event, outcome = outcome)).also { persist(it) }
    }

    @Synchronized
    fun confirm(accountRef: String?, state: PurchaseState) {
        val record = current() ?: error("PURCHASE_STATE_INVALID")
        val order = record.order ?: error("PURCHASE_STATE_INVALID")
        check(accountRef != null && accountRef == order.accountRef && state.phase == PurchaseFlow.CONFIRMED &&
            state.payment == order.payment && state.payment?.paymentStatus == "paid" && state.freshMeConfirmed &&
            state.payment.creditState !in setOf("needs_review", "unapplied") &&
            (state.payment.creditState == null || state.confirmationPlansLoaded) &&
            (state.payment.product == null || state.confirmationPlansLoaded))
        persist(record.copy(order = order.copy(outcome = "confirmed")))
    }

    /** Resolve only the exact captured create; disk CAS also fences concurrent store writers. */
    @Synchronized
    fun resolveExpiredNoOrder(
        flight: PurchaseFlight, activeAttempt: String, accountRef: String?, installationId: String,
        event: String,
    ): PurchaseAttempt {
        val proof = PaymentsContract.parse(JSONObject(event)) as? PaymentsEvent.Failure
        check(proof?.expiredNoOrder == true) { "PURCHASE_NO_CREATE_INVALID" }
        val captured = flight.paymentIntent ?: error("PURCHASE_NO_CREATE_UNCORRELATED")
        check(flight.kind == PurchaseFlightKind.PAYMENT && flight.attempt == activeAttempt &&
            JSONObject(event).getString("attempt_id") == activeAttempt &&
            flight.installationId == installationId && installationId.matches(Regex("^[0-9a-f]{64}$")) &&
            accountRef != null && captured.order?.accountRef == accountRef && captured.unresolved &&
            captured.order.paymentEvent == null && captured.frozenQuote != null &&
            flight.quoteId == captured.quoteId && flight.idempotencyKey == captured.paymentKey) {
            "PURCHASE_NO_CREATE_UNCORRELATED"
        }
        val expected = store.read() ?: error("PURCHASE_STATE_INVALID")
        val current = PurchaseAttemptCodec.decode(expected) ?: error("PURCHASE_STATE_INVALID")
        val resolved = captured.copy(order = captured.order.copy(outcome = "expired_no_order",
            noCreateEvent = event, noCreateInstallationId = installationId))
        // A duplicate may only be a no-op for this exact committed result, never for a later attempt.
        if (current == resolved) return current
        check(current == captured) { "PURCHASE_NO_CREATE_STALE" }
        store.resolveNoOrder(expected, PurchaseAttemptCodec.encode(resolved), installationId)
        return resolved
    }

    /** Redact all order details until the current account has been freshly accepted. */
    fun recoveryState(accountRef: String?): PurchaseState? = try {
        val record = current()
        when {
            record == null -> null
            record.order?.outcome == "expired_no_order" ->
                if (accountRef != null && record.order.accountRef == accountRef)
                    PurchaseFlow.expiredNoOrderState(null, accountRef) else null
            !record.unresolved -> null
            record.legacyUncertain -> PurchaseState(recovery = "legacy_unknown", phase = PurchaseFlow.ERROR)
            accountRef == null -> PurchaseState(recovery = "verify_account", phase = PurchaseFlow.AWAITING_PAYMENT)
            record.order?.accountRef != accountRef -> PurchaseState(recovery = "account_mismatch", phase = PurchaseFlow.ERROR)
            else -> {
                val order = record.order ?: error("PURCHASE_STATE_INVALID")
                val state = PurchaseState(phase = PurchaseFlow.AWAITING_PAYMENT, ownerAccountRef = accountRef,
                    recovery = if (order.payment == null) "unknown_create" else "known_payment",
                    quote = record.frozenQuote, selectedPlanId = record.planId, selectedMethod = record.method,
                    selectedRenewExtraSlotIds = record.selection?.renewExtraSlotIds.orEmpty())
                order.payment?.let { PurchaseFlow.paymentResult(state, it) } ?: state
            }
        }
    } catch (_: Exception) { PurchaseState(recovery = "unreadable", phase = PurchaseFlow.ERROR) }

    internal fun generate(): String {
        val key = newKey()
        require(PurchaseAttemptCodec.usableKey(key)) { "PURCHASE_KEY_INVALID" }
        return key
    }

    private fun persist(attempt: PurchaseAttempt) {
        store.write(PurchaseAttemptCodec.encode(attempt))
    }
}

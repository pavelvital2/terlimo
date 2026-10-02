package xyz.terlimo.test

import org.json.JSONArray
import org.json.JSONObject
import java.time.Instant

/**
 * Host projection of the native payment bridge vocabulary. V1 and negotiated V2
 * retain schema_version 1.0 and are distinguished by their exact field sets.
 * This is a strict read of the events native emits:
 * every accepted key must be exactly the contract key set, required fields must be
 * present and enums are enforced. A malformed event is rejected and the last good
 * projection is kept; nothing is completed, coerced or fabricated.
 *
 * The contract carries no QR field and no checkout capability field: the host must
 * never synthesize either. `checkout_reference` (string|null) and
 * `credited_entitlement_revision` (string|null) are forwarded verbatim by native and
 * are display data only; this parser never derives eligibility or a grant.
 */
internal data class PaymentPlan(
    val planId: String,
    val title: String,
    val durationCode: String,
    val baseDeviceLimit: Int,
    val amountMinor: Long,
    val currency: String,
    val methods: List<String>,
    val product: PaymentProduct? = null,
)

internal data class PaymentQuote(
    val quoteId: String,
    val amountMinor: Long,
    val currency: String,
    val durationCode: String,
    val deviceLimit: Int,
    val method: String,
    val expiresAt: String,
    val product: PaymentProduct? = null,
)

internal data class PaymentStatusView(
    val paymentId: String,
    val paymentStatus: String,
    val checkoutReference: String?,
    val creditedEntitlementRevision: String?,
    val accessApplicationState: String,
    val product: PaymentProduct? = null,
    val creditState: String? = null,
    val creditReviewReason: String? = null,
    val creditedProduct: CreditedPaymentProduct? = null,
)

internal sealed class PaymentsEvent {
    data class Plans(val plansRevision: String, val plans: List<PaymentPlan>) : PaymentsEvent()
    data class Quote(val quote: PaymentQuote) : PaymentsEvent()
    /** `type` is the parsed wire type: exactly TYPE_PAYMENT_CREATE_RESULT or TYPE_PAYMENT_GET_RESULT. */
    data class Payment(val type: String, val payment: PaymentStatusView) : PaymentsEvent()
    data class Failure(val type: String, val code: String) : PaymentsEvent()
}

internal object PaymentsContract {
    const val TYPE_PLANS_LIST_RESULT = "plans_list_result"
    const val TYPE_QUOTE_CREATE_RESULT = "quote_create_result"
    const val TYPE_PAYMENT_CREATE_RESULT = "payment_create_result"
    const val TYPE_PAYMENT_GET_RESULT = "payment_get_result"

    /** Frozen host actions; the host sends nothing outside this set for payments. */
    const val ACTION_PLANS_LIST = "plans_list"
    const val ACTION_QUOTE_CREATE = "quote_create"
    const val ACTION_PAYMENT_CREATE = "payment_create"
    const val ACTION_PAYMENT_GET = "payment_get"

    /** Bounded native classification codes plus the frozen schemas/errors.json enum. */
    val ERROR_CODES: Set<String> = setOf(
        "INVALID_REQUEST", "MOBILE_STATE_UNAVAILABLE", "BUSY", "TRANSPORT", "PROVIDER_UNAVAILABLE",
        "BAD_MESSAGE", "UNSUPPORTED_VERSION", "UNKNOWN_CRITICAL_FIELD", "PROOF_INVALID",
        "CHALLENGE_EXPIRED", "CHALLENGE_REUSED", "REPLAY_DETECTED", "WRONG_ENVIRONMENT",
        "WRONG_SCOPE", "SESSION_INVALID", "SESSION_EXPIRED", "TELEGRAM_REQUIRED",
        "TELEGRAM_LINK_PENDING", "TELEGRAM_LINK_REJECTED", "TRIAL_USED",
        "CHANNEL_CONFIRMATION_REQUIRED", "DEVICE_LIMIT_REACHED", "APPROVAL_REQUIRED",
        "DEVICE_REVOKED", "SUBSCRIPTION_EXPIRED", "SUBSCRIPTION_MISSING",
        "IDEMPOTENCY_CONFLICT", "REVISION_CONFLICT", "ACCESS_SYNC_PENDING", "OPERATION_PENDING",
        "OPERATION_FAILED", "OPERATION_UNKNOWN", "LEASE_CONFLICT", "GRANT_MISSING",
        "NODE_UNAVAILABLE", "READBACK_FAILED", "PAYMENT_NOT_FOUND", "PAYMENT_STATE_INVALID",
        "QUOTE_EXPIRED", "METHOD_UNAVAILABLE", "CHECKOUT_POLICY_DENIED", "RATE_LIMITED",
        "SERVICE_UNAVAILABLE", "NOT_FOUND", "ACCESS_DENIED", "INTERNAL",
    )

    val DURATION_CODES: Set<String> = setOf("days:30", "months:3", "months:6")
    val METHODS: Set<String> = setOf("card", "sbp", "crypto")
    val PAYMENT_STATUSES: Set<String> = setOf(
        "created", "pending", "paid", "failed", "expired", "refunded", "disputed",
    )
    val ACCESS_APPLICATION_STATES: Set<String> =
        setOf("not_requested", "pending", "applied", "retryable_failure", "rejected")
    val CREDIT_STATES: Set<String> = setOf("unapplied", "applied", "needs_review")
    val CREDIT_REVIEW_REASONS: Set<String> = setOf(
        "owner_unbound", "owner_changed", "target_unavailable", "target_expired",
        "target_period_changed", "extra_slot_unavailable",
    )

    private val ENVELOPE = setOf("v", "attempt_id", "type", "state")
    private val PLANS_KEYS = ENVELOPE + setOf(
        "request_id", "server_time", "schema_version", "plans_revision", "plans",
    )
    private val QUOTE_KEYS = ENVELOPE + setOf(
        "request_id", "server_time", "schema_version", "quote_id", "amount", "duration_code",
        "device_limit", "method", "expires_at",
    )
    private val PAYMENT_KEYS = ENVELOPE + setOf(
        "request_id", "server_time", "schema_version", "payment_id", "payment_status",
        "checkout_reference", "credited_entitlement_revision", "access_application_state",
    )
    private val ERROR_KEYS = ENVELOPE + setOf("code")
    private val MONEY_KEYS = setOf("amount_minor", "currency")
    private val PLAN_KEYS = setOf(
        "plan_id", "title", "duration_code", "base_device_limit", "amount", "methods",
    )
    private val QUOTE_V2_KEYS = QUOTE_KEYS + "product"
    private val PAYMENT_V2_KEYS = PAYMENT_KEYS + setOf(
        "product", "credit_state", "credit_review_reason", "credited_product",
    )
    private val PLAN_V2_KEYS = PLAN_KEYS + "product"
    private val PRODUCT_KEYS = setOf(
        "kind", "plan_id", "device_delta", "target_entitlement_id", "target_valid_until",
        "valid_from", "valid_until", "renew_extra_slot_ids", "base_amount_minor",
        "extra_amount_minor", "device_limit", "extra_slots",
    )
    private val EXTRA_SLOT_KEYS = setOf("slot_id", "expires_at", "renew_amount_minor")
    private val CREDITED_PRODUCT_KEYS = setOf(
        "valid_from", "valid_until", "device_limit", "current_device_limit",
    )
    private val PRODUCT_PLAN_IDS = setOf(
        "terlimo-30d", "terlimo-3m", "terlimo-6m", "terlimo-extra-device",
    )

    private val REQUEST_ID = Regex("^[0-9a-f]{32}$")
    private val UTC_TIME = Regex("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$")
    private val REVISION = Regex("^(0|[1-9][0-9]{0,18})$")
    private val CURRENCY = Regex("^[A-Z]{3}$")
    private val UUID = Regex("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")

    fun parse(event: JSONObject): PaymentsEvent {
        try {
            val type = event.getString("type")
            val state = event.getString("state")
            val keys = event.keys().asSequence().toSet()
            return when (state) {
                "error" -> {
                    check(keys == ERROR_KEYS) { "PAYMENTS_INVALID" }
                    val code = event.getString("code")
                    check(code in ERROR_CODES) { "PAYMENTS_INVALID" }
                    PaymentsEvent.Failure(type, code)
                }
                "ok" -> when (type) {
                    TYPE_PLANS_LIST_RESULT -> parsePlans(event, keys)
                    TYPE_QUOTE_CREATE_RESULT -> parseQuote(event, keys)
                    TYPE_PAYMENT_CREATE_RESULT, TYPE_PAYMENT_GET_RESULT -> parsePayment(type, event, keys)
                    else -> error("PAYMENTS_INVALID")
                }
                else -> error("PAYMENTS_INVALID")
            }
        } catch (error: IllegalStateException) {
            throw error
        } catch (error: Exception) {
            throw IllegalStateException("PAYMENTS_INVALID", error)
        }
    }

    private fun parsePlans(event: JSONObject, keys: Set<String>): PaymentsEvent.Plans {
        check(keys == PLANS_KEYS) { "PAYMENTS_INVALID" }
        val revision = event.getString("plans_revision")
        check(revision.matches(REVISION)) { "PAYMENTS_INVALID" }
        val raw = event.getJSONArray("plans")
        val plans = ArrayList<PaymentPlan>(raw.length())
        var planKeys: Set<String>? = null
        for (index in 0 until raw.length()) {
            val plan = raw.getJSONObject(index)
            val itemKeys = plan.keys().asSequence().toSet()
            check(planKeys == null || planKeys == itemKeys) { "PAYMENTS_INVALID" }
            planKeys = itemKeys
            plans += parsePlan(plan)
        }
        validateEnvelope(event, planKeys == PLAN_V2_KEYS)
        if (planKeys == PLAN_V2_KEYS) strictString(event, "plans_revision")
        return PaymentsEvent.Plans(revision, plans)
    }

    private fun parsePlan(plan: JSONObject): PaymentPlan {
        val keys = plan.keys().asSequence().toSet()
        val v2 = keys == PLAN_V2_KEYS
        check(v2 || keys == PLAN_KEYS) { "PAYMENTS_INVALID" }
        val planId = if (v2) strictString(plan, "plan_id") else plan.getString("plan_id")
        val title = if (v2) strictString(plan, "title") else plan.getString("title")
        val duration = if (v2) strictString(plan, "duration_code") else plan.getString("duration_code")
        check(planId.isNotEmpty() && planId.length <= 128 && title.length <= 128 &&
            validDuration(duration, v2)) { "PAYMENTS_INVALID" }
        val limit = if (v2) strictInt(plan, "base_device_limit") else plan.getInt("base_device_limit")
        check(if (v2) limit == 2 else limit in 1..100) { "PAYMENTS_INVALID" }
        val (amountMinor, currency) = parseMoney(plan.getJSONObject("amount"))
        check(!v2 || (amountMinor > 0 && currency == "RUB")) { "PAYMENTS_INVALID" }
        val methods = parseMethods(plan.getJSONArray("methods"))
        val product = if (v2) nullableProduct(plan) else null
        check(product == null || product.planId == planId) { "PAYMENTS_INVALID" }
        return PaymentPlan(planId, title, duration, limit, amountMinor, currency, methods, product)
    }

    private fun parseQuote(event: JSONObject, keys: Set<String>): PaymentsEvent.Quote {
        val v2 = keys == QUOTE_V2_KEYS
        check(v2 || keys == QUOTE_KEYS) { "PAYMENTS_INVALID" }
        validateEnvelope(event, v2)
        val quoteId = if (v2) strictUuid(event, "quote_id") else event.getString("quote_id")
        val duration = if (v2) strictString(event, "duration_code") else event.getString("duration_code")
        val method = if (v2) strictString(event, "method") else event.getString("method")
        val expiresAt = if (v2) strictUtc(event, "expires_at") else event.getString("expires_at")
        check(quoteId.isNotEmpty() && quoteId.length <= 128 &&
            validDuration(duration, v2) && method in METHODS && expiresAt.matches(UTC_TIME)) {
            "PAYMENTS_INVALID"
        }
        val limit = if (v2) strictInt(event, "device_limit") else event.getInt("device_limit")
        check(if (v2) limit >= 2 else limit in 1..100) { "PAYMENTS_INVALID" }
        val (amountMinor, currency) = parseMoney(event.getJSONObject("amount"))
        check(!v2 || (amountMinor > 0 && currency == "RUB")) { "PAYMENTS_INVALID" }
        val product = if (v2) nullableProduct(event) else null
        check(product == null || product.deviceLimit == limit) { "PAYMENTS_INVALID" }
        return PaymentsEvent.Quote(PaymentQuote(
            quoteId = quoteId, amountMinor = amountMinor, currency = currency, durationCode = duration,
            deviceLimit = limit, method = method, expiresAt = expiresAt, product = product,
        ))
    }

    private fun parsePayment(type: String, event: JSONObject, keys: Set<String>): PaymentsEvent.Payment {
        val v2 = keys == PAYMENT_V2_KEYS
        check(v2 || keys == PAYMENT_KEYS) { "PAYMENTS_INVALID" }
        validateEnvelope(event, v2)
        val paymentId = if (v2) strictUuid(event, "payment_id") else event.getString("payment_id")
        val status = if (v2) strictString(event, "payment_status") else event.getString("payment_status")
        val applicationState = if (v2) strictString(event, "access_application_state") else event.getString("access_application_state")
        check(paymentId.isNotEmpty() && paymentId.length <= 128 &&
            status in PAYMENT_STATUSES && applicationState in ACCESS_APPLICATION_STATES) {
            "PAYMENTS_INVALID"
        }
        val reference = nullableBoundedString(event, "checkout_reference", 256)
        val credited = nullableBoundedString(event, "credited_entitlement_revision", 128)
        val product = if (v2) nullableProduct(event) else null
        val creditState = if (v2) strictString(event, "credit_state").also {
            check(it in CREDIT_STATES) { "PAYMENTS_INVALID" }
        } else null
        val reviewReason = if (v2) nullableEnum(event, "credit_review_reason", CREDIT_REVIEW_REASONS) else null
        val creditedProduct = if (v2 && !event.isNull("credited_product")) {
            parseCreditedProduct(event.getJSONObject("credited_product"))
        } else null
        return PaymentsEvent.Payment(type, PaymentStatusView(
            paymentId = paymentId, paymentStatus = status, checkoutReference = reference,
            creditedEntitlementRevision = credited, accessApplicationState = applicationState,
            product = product, creditState = creditState, creditReviewReason = reviewReason,
            creditedProduct = creditedProduct,
        ))
    }

    private fun validateEnvelope(event: JSONObject, v2: Boolean = false) {
        if (v2) {
            check(strictInt(event, "v") == 1 && strictString(event, "request_id").matches(REQUEST_ID) &&
                validUtc(strictString(event, "server_time")) &&
                strictString(event, "schema_version") == "1.0") { "PAYMENTS_INVALID" }
            return
        }
        val requestId = event.getString("request_id")
        val serverTime = event.getString("server_time")
        check(event.getInt("v") == 1 && requestId.matches(REQUEST_ID) &&
            serverTime.matches(UTC_TIME) && event.getString("schema_version") == "1.0") {
            "PAYMENTS_INVALID"
        }
    }

    private fun nullableProduct(source: JSONObject): PaymentProduct? =
        if (source.isNull("product")) null else parseProduct(source.getJSONObject("product"))

    private fun parseProduct(product: JSONObject): PaymentProduct {
        check(product.keys().asSequence().toSet() == PRODUCT_KEYS) { "PAYMENTS_INVALID" }
        val kind = strictString(product, "kind")
        val planId = strictString(product, "plan_id")
        val delta = strictInt(product, "device_delta")
        check(planId in PRODUCT_PLAN_IDS &&
            ((kind == "subscription" && delta == 0 && planId != "terlimo-extra-device") ||
                (kind == "device_addon" && delta == 1 && planId == "terlimo-extra-device"))) {
            "PAYMENTS_INVALID"
        }
        val targetId = if (product.isNull("target_entitlement_id")) null
            else strictUuid(product, "target_entitlement_id")
        val targetUntil = if (product.isNull("target_valid_until")) null
            else strictUtc(product, "target_valid_until")
        val validFrom = strictUtc(product, "valid_from")
        val validUntil = strictUtc(product, "valid_until")
        check(Instant.parse(validUntil).isAfter(Instant.parse(validFrom))) { "PAYMENTS_INVALID" }
        val rawIds = product.getJSONArray("renew_extra_slot_ids")
        val ids = ArrayList<String>(rawIds.length())
        val seenIds = HashSet<String>()
        for (index in 0 until rawIds.length()) {
            val raw = rawIds.get(index)
            check(raw is String && raw.matches(UUID) && seenIds.add(raw.lowercase())) {
                "PAYMENTS_INVALID"
            }
            ids += raw
        }
        val baseAmount = strictLong(product, "base_amount_minor")
        val extraAmount = strictLong(product, "extra_amount_minor")
        val limit = strictInt(product, "device_limit")
        check(baseAmount >= 0 && extraAmount >= 0 && limit >= 2) { "PAYMENTS_INVALID" }
        check(if (kind == "subscription") limit.toLong() == 2L + ids.size else
            baseAmount == 0L && targetId != null && targetUntil != null && validUntil == targetUntil && ids.isEmpty()) {
            "PAYMENTS_INVALID"
        }
        val rawSlots = product.getJSONArray("extra_slots")
        val slots = ArrayList<PaymentExtraSlot>(rawSlots.length())
        val seenSlots = HashSet<String>()
        for (index in 0 until rawSlots.length()) {
            val slot = rawSlots.getJSONObject(index)
            check(slot.keys().asSequence().toSet() == EXTRA_SLOT_KEYS) { "PAYMENTS_INVALID" }
            val slotId = strictUuid(slot, "slot_id")
            check(seenSlots.add(slotId.lowercase())) { "PAYMENTS_INVALID" }
            val expiresAt = strictUtc(slot, "expires_at")
            val renewAmount = if (slot.isNull("renew_amount_minor")) null
                else strictLong(slot, "renew_amount_minor").also {
                    check(it > 0) { "PAYMENTS_INVALID" }
                }
            slots += PaymentExtraSlot(slotId, expiresAt, renewAmount)
        }
        return PaymentProduct(kind, planId, delta, targetId, targetUntil, validFrom, validUntil,
            ids, baseAmount, extraAmount, limit, slots)
    }

    private fun parseCreditedProduct(product: JSONObject): CreditedPaymentProduct {
        check(product.keys().asSequence().toSet() == CREDITED_PRODUCT_KEYS) { "PAYMENTS_INVALID" }
        val from = strictUtc(product, "valid_from")
        val until = if (product.isNull("valid_until")) null else strictUtc(product, "valid_until")
        val limit = strictInt(product, "device_limit")
        val currentLimit = strictInt(product, "current_device_limit")
        check(limit >= 2 && currentLimit >= 2) { "PAYMENTS_INVALID" }
        return CreditedPaymentProduct(from, until, limit, currentLimit)
    }

    private fun validDuration(duration: String, v2: Boolean): Boolean =
        duration in DURATION_CODES ||
            (v2 && duration.startsWith("until:") && validUtc(duration.removePrefix("until:")))

    private fun validUtc(value: String): Boolean = value.matches(UTC_TIME) &&
        runCatching { Instant.parse(value) }.isSuccess

    private fun strictString(source: JSONObject, key: String): String {
        val value = source.get(key)
        check(value is String) { "PAYMENTS_INVALID" }
        return value
    }

    private fun strictUuid(source: JSONObject, key: String): String =
        strictString(source, key).also { check(it.matches(UUID)) { "PAYMENTS_INVALID" } }

    private fun strictUtc(source: JSONObject, key: String): String =
        strictString(source, key).also { check(validUtc(it)) { "PAYMENTS_INVALID" } }

    private fun strictLong(source: JSONObject, key: String): Long {
        val value = source.get(key)
        check(value is Int || value is Long) { "PAYMENTS_INVALID" }
        return (value as Number).toLong()
    }

    private fun strictInt(source: JSONObject, key: String): Int =
        strictLong(source, key).also {
            check(it in Int.MIN_VALUE.toLong()..Int.MAX_VALUE.toLong()) { "PAYMENTS_INVALID" }
        }.toInt()

    private fun nullableEnum(source: JSONObject, key: String, allowed: Set<String>): String? =
        if (source.isNull(key)) null else strictString(source, key).also {
            check(it in allowed) { "PAYMENTS_INVALID" }
        }

    private fun parseMoney(money: JSONObject): Pair<Long, String> {
        check(money.keys().asSequence().toSet() == MONEY_KEYS) { "PAYMENTS_INVALID" }
        val raw = money.opt("amount_minor")
        check(raw is Int || raw is Long) { "PAYMENTS_INVALID" }
        val amountMinor = (raw as Number).toLong()
        check(amountMinor >= 0) { "PAYMENTS_INVALID" }
        val currency = money.getString("currency")
        check(currency.matches(CURRENCY)) { "PAYMENTS_INVALID" }
        return amountMinor to currency
    }

    private fun parseMethods(array: JSONArray): List<String> {
        val methods = ArrayList<String>(array.length())
        val seen = HashSet<String>()
        for (index in 0 until array.length()) {
            val method = array.getString(index)
            check(method in METHODS && seen.add(method)) { "PAYMENTS_INVALID" }
            methods += method
        }
        return methods
    }

    private fun nullableBoundedString(source: JSONObject, key: String, maxLength: Int): String? {
        if (!source.has(key) || source.isNull(key)) return null
        val value = source.get(key)
        check(value is String && value.length <= maxLength) { "PAYMENTS_INVALID" }
        return value
    }
}

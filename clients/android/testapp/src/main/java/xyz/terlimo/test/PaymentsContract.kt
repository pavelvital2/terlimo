package xyz.terlimo.test

import org.json.JSONArray
import org.json.JSONObject

/**
 * S5 host projection of the frozen native payment bridge vocabulary (cdbef94,
 * go_client/terlimo_payments.go). This is a strict read of the events native emits:
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
)

internal data class PaymentQuote(
    val quoteId: String,
    val amountMinor: Long,
    val currency: String,
    val durationCode: String,
    val deviceLimit: Int,
    val method: String,
    val expiresAt: String,
)

internal data class PaymentStatusView(
    val paymentId: String,
    val paymentStatus: String,
    val checkoutReference: String?,
    val creditedEntitlementRevision: String?,
    val accessApplicationState: String,
)

internal sealed class PaymentsEvent {
    data class Plans(val plansRevision: String, val plans: List<PaymentPlan>) : PaymentsEvent()
    data class Quote(val quote: PaymentQuote) : PaymentsEvent()
    data class Payment(val payment: PaymentStatusView) : PaymentsEvent()
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

    private val REQUEST_ID = Regex("^[0-9a-f]{32}$")
    private val UTC_TIME = Regex("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$")
    private val REVISION = Regex("^(0|[1-9][0-9]{0,18})$")
    private val CURRENCY = Regex("^[A-Z]{3}$")

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
                    TYPE_PAYMENT_CREATE_RESULT, TYPE_PAYMENT_GET_RESULT -> parsePayment(event, keys)
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
        validateEnvelope(event)
        val revision = event.getString("plans_revision")
        check(revision.matches(REVISION)) { "PAYMENTS_INVALID" }
        val raw = event.getJSONArray("plans")
        val plans = ArrayList<PaymentPlan>(raw.length())
        for (index in 0 until raw.length()) {
            plans += parsePlan(raw.getJSONObject(index))
        }
        return PaymentsEvent.Plans(revision, plans)
    }

    private fun parsePlan(plan: JSONObject): PaymentPlan {
        check(plan.keys().asSequence().toSet() == PLAN_KEYS) { "PAYMENTS_INVALID" }
        val planId = plan.getString("plan_id")
        val title = plan.getString("title")
        val duration = plan.getString("duration_code")
        check(planId.isNotEmpty() && planId.length <= 128 && title.length <= 128 &&
            duration in DURATION_CODES) { "PAYMENTS_INVALID" }
        val limit = plan.getInt("base_device_limit")
        check(limit in 1..100) { "PAYMENTS_INVALID" }
        val (amountMinor, currency) = parseMoney(plan.getJSONObject("amount"))
        val methods = parseMethods(plan.getJSONArray("methods"))
        return PaymentPlan(planId, title, duration, limit, amountMinor, currency, methods)
    }

    private fun parseQuote(event: JSONObject, keys: Set<String>): PaymentsEvent.Quote {
        check(keys == QUOTE_KEYS) { "PAYMENTS_INVALID" }
        validateEnvelope(event)
        val quoteId = event.getString("quote_id")
        val duration = event.getString("duration_code")
        val method = event.getString("method")
        val expiresAt = event.getString("expires_at")
        check(quoteId.isNotEmpty() && quoteId.length <= 128 &&
            duration in DURATION_CODES && method in METHODS && expiresAt.matches(UTC_TIME)) {
            "PAYMENTS_INVALID"
        }
        val limit = event.getInt("device_limit")
        check(limit in 1..100) { "PAYMENTS_INVALID" }
        val (amountMinor, currency) = parseMoney(event.getJSONObject("amount"))
        return PaymentsEvent.Quote(PaymentQuote(
            quoteId = quoteId, amountMinor = amountMinor, currency = currency, durationCode = duration,
            deviceLimit = limit, method = method, expiresAt = expiresAt,
        ))
    }

    private fun parsePayment(event: JSONObject, keys: Set<String>): PaymentsEvent.Payment {
        check(keys == PAYMENT_KEYS) { "PAYMENTS_INVALID" }
        validateEnvelope(event)
        val paymentId = event.getString("payment_id")
        val status = event.getString("payment_status")
        val applicationState = event.getString("access_application_state")
        check(paymentId.isNotEmpty() && paymentId.length <= 128 &&
            status in PAYMENT_STATUSES && applicationState in ACCESS_APPLICATION_STATES) {
            "PAYMENTS_INVALID"
        }
        val reference = nullableBoundedString(event, "checkout_reference", 256)
        val credited = nullableBoundedString(event, "credited_entitlement_revision", 128)
        return PaymentsEvent.Payment(PaymentStatusView(
            paymentId = paymentId, paymentStatus = status, checkoutReference = reference,
            creditedEntitlementRevision = credited, accessApplicationState = applicationState,
        ))
    }

    private fun validateEnvelope(event: JSONObject) {
        val requestId = event.getString("request_id")
        val serverTime = event.getString("server_time")
        check(event.getInt("v") == 1 && requestId.matches(REQUEST_ID) &&
            serverTime.matches(UTC_TIME) && event.getString("schema_version") == "1.0") {
            "PAYMENTS_INVALID"
        }
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

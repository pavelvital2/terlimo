package xyz.terlimo.test

import org.json.JSONObject
import java.time.Instant

internal data class ReferralCandidate(val id: String, val code: String)
internal data class ReferralPendingResponse(val requestId: String, val candidate: ReferralCandidate)
internal data class ReferralClearedResponse(val requestId: String)
internal data class ReferralAttribution(val state: String, val receiptId: String?, val reason: String?)
internal data class ReferralDiscount(val currency: String, val amountMinor: Long, val state: String)
internal data class ReferralInfo(
    val accountRef: String,
    val code: String,
    val telegramLink: String,
    val webLink: String,
    val attribution: ReferralAttribution,
    val trialBonusDays: Long,
    val discount: ReferralDiscount,
    val waitingDays: Long,
    val appliedDays: Long,
    val termsVersion: String,
)
internal data class ReferralInfoResponse(val requestId: String, val referral: ReferralInfo)
internal data class ReferralRegistrationCorrelation(
    val candidateId: String, val registrationId: String, val idempotencyKey: String,
)
internal data class ReferralRegistrationPending(
    val correlation: ReferralRegistrationCorrelation,
    val token: String, val botUsername: String, val deepLink: String,
    val expiresAt: String, val expiresIn: Int,
)
/** Exactly the published full wire receipt: installation and Telegram token are host context. */
internal data class ReferralAttributionReceipt(
    val receiptId: String, val accountRef: String, val candidateId: String,
    val registrationId: String, val idempotencyKey: String,
    val state: String, val reason: String? = null,
)
internal sealed class ReferralRegistrationResult {
    data class Pending(val requestId: String, val serverTime: String,
        val pending: ReferralRegistrationPending) : ReferralRegistrationResult()
    data class Registered(val requestId: String, val serverTime: String,
        val receipt: ReferralAttributionReceipt) : ReferralRegistrationResult()
}
internal data class ReferralRegistrationExpiry(
    val requestId: String, val serverTime: String, val correlation: ReferralRegistrationCorrelation,
    val httpStatus: Int = 410,
)

/** Exact server envelopes from contract 27. Native framing is deliberately a separate seam. */
internal object ReferralContract {
    private val CODE = Regex("^[A-Za-z0-9]{1,32}$")
    private val UUID = Regex("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
    private val REQUEST_ID = Regex("^[0-9a-f]{32}$")
    private val UTC_TIME = Regex("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$")
    const val TERMS_VERSION = "referral-20261003-v1"
    val REJECTION_REASONS = setOf("self", "already_attributed", "ineligible", "invalid")
    val ERROR_CODES = setOf(
        "REFERRAL_CODE_INVALID", "REFERRAL_CANDIDATE_LOCKED", "REFERRAL_HISTORY_PENDING",
        "BAD_MESSAGE", "IDEMPOTENCY_CONFLICT", "SESSION_INVALID", "SESSION_EXPIRED",
        "TELEGRAM_REQUIRED", "DEVICE_REVOKED", "ACCESS_DENIED", "RATE_LIMITED",
        "SERVICE_UNAVAILABLE", "INTERNAL", "TRANSPORT", "MOBILE_STATE_UNAVAILABLE",
        "OPERATION_UNKNOWN", "BUSY",
        "REGISTRATION_EXPIRED", "REFERRAL_REGISTRATION_UNAVAILABLE",
    )

    fun validCode(code: String): Boolean = CODE.matches(code)
    fun validUuid(value: String): Boolean = UUID.matches(value)
    fun validRequestId(value: String): Boolean = REQUEST_ID.matches(value)
    fun validOperationKey(value: String): Boolean = value.length in 16..128 &&
        value.none { it.code < 32 || it.code == 127 }
    fun validUtcTime(value: String): Boolean = UTC_TIME.matches(value) && runCatching { Instant.parse(value) }.isSuccess

    /** Keyed path only. A legacy registered/receipt-id response cannot complete this flow. */
    fun parseRegistration(envelope: JSONObject): ReferralRegistrationResult = strict {
        keys(envelope, "request_id", "server_time", "schema_version", "status", "registration")
        check(string(envelope, "schema_version") == "1.0" && string(envelope, "status") == "ok")
        val requestId = requestId(envelope)
        val time = utc(envelope, "server_time")
        val registration = envelope.getJSONObject("registration")
        when (string(registration, "state")) {
            "pending" -> {
                keys(registration, "state", "token", "bot_username", "deep_link", "expires_at", "expires_in", "referral_registration")
                val token = string(registration, "token", 256)
                val bot = string(registration, "bot_username", 64)
                val link = string(registration, "deep_link", 4096)
                val expiresIn = nonNegativeLong(registration, "expires_in")
                check(expiresIn in 1L..3600L && link == "https://t.me/$bot?start=$token")
                ReferralRegistrationResult.Pending(requestId, time, ReferralRegistrationPending(
                    parseCorrelation(registration.getJSONObject("referral_registration")), token, bot, link,
                    utc(registration, "expires_at"), expiresIn.toInt()))
            }
            "registered" -> {
                val expected = setOf("state", "referral_attribution")
                val actual = registration.keys().asSequence().toSet()
                check(actual == expected || actual == expected + "referral_attribution_receipt_id")
                val receipt = parseReceipt(registration.getJSONObject("referral_attribution"))
                if (registration.has("referral_attribution_receipt_id")) {
                    check(string(registration, "referral_attribution_receipt_id") == receipt.receiptId)
                }
                ReferralRegistrationResult.Registered(requestId, time, receipt)
            }
            else -> error("REFERRAL_INVALID")
        }
    }

    /** HTTP410, complete details, retryable=false; generic status/errors carry no terminality. */
    fun parseRegistrationExpiry(envelope: JSONObject, httpStatus: Int): ReferralRegistrationExpiry = strict {
        check(httpStatus == 410)
        keys(envelope, "request_id", "server_time", "schema_version", "status", "code", "retryable", "details")
        check(string(envelope, "schema_version") == "1.0" && string(envelope, "status") == "error" &&
            string(envelope, "code") == "REGISTRATION_EXPIRED" && envelope.get("retryable") == false)
        val details = envelope.getJSONObject("details")
        keys(details, "state", "referral_registration")
        check(string(details, "state") == "expired")
        ReferralRegistrationExpiry(requestId(envelope), utc(envelope, "server_time"),
            parseCorrelation(details.getJSONObject("referral_registration")), httpStatus)
    }

    internal fun parseCorrelation(value: JSONObject): ReferralRegistrationCorrelation = strict {
        keys(value, "candidate_id", "registration_id", "idempotency_key")
        ReferralRegistrationCorrelation(string(value, "candidate_id"), string(value, "registration_id"),
            string(value, "idempotency_key")).also(::validateCorrelation)
    }
    internal fun parseReceipt(value: JSONObject): ReferralAttributionReceipt = strict {
        keys(value, "receipt_id", "account_ref", "candidate_id", "registration_id", "idempotency_key", "state", "reason")
        ReferralAttributionReceipt(string(value, "receipt_id"), string(value, "account_ref"),
            string(value, "candidate_id"), string(value, "registration_id"), string(value, "idempotency_key"),
            string(value, "state"), nullableString(value, "reason")).also(::validateReceipt)
    }
    internal fun validateCorrelation(value: ReferralRegistrationCorrelation) {
        check(validUuid(value.candidateId) && validUuid(value.registrationId) && validOperationKey(value.idempotencyKey)) {
            "REFERRAL_INVALID"
        }
    }
    internal fun validatePending(value: ReferralRegistrationPending) {
        validateCorrelation(value.correlation)
        check(value.token.length in 1..256 && value.botUsername.length in 1..64 &&
            value.token.none { it.code < 32 || it.code == 127 } && value.botUsername.none { it.code < 32 || it.code == 127 } &&
            value.deepLink == "https://t.me/${value.botUsername}?start=${value.token}" &&
            value.expiresIn in 1..3600 && validUtcTime(value.expiresAt)) { "REFERRAL_INVALID" }
    }
    internal fun validateReceipt(value: ReferralAttributionReceipt) {
        validateCorrelation(ReferralRegistrationCorrelation(value.candidateId, value.registrationId, value.idempotencyKey))
        check(validUuid(value.receiptId) && validUuid(value.accountRef) &&
            ((value.state == "attached" && value.reason == null) ||
             (value.state == "rejected" && value.reason in REJECTION_REASONS))) { "REFERRAL_INVALID" }
    }
    internal fun validateExpiry(value: ReferralRegistrationExpiry) {
        validateCorrelation(value.correlation)
        check(value.httpStatus == 410 && validRequestId(value.requestId) && validUtcTime(value.serverTime)) { "REFERRAL_INVALID" }
    }

    fun parsePending(envelope: JSONObject): ReferralPendingResponse = strict {
        keys(envelope, "request_id", "candidate")
        val candidate = envelope.getJSONObject("candidate")
        keys(candidate, "id", "state", "code")
        check(string(candidate, "state") == "pending")
        ReferralPendingResponse(requestId(envelope), ReferralCandidate(
            string(candidate, "id").also { check(UUID.matches(it)) }, code(candidate, "code")))
    }

    fun parseCleared(envelope: JSONObject): ReferralClearedResponse = strict {
        keys(envelope, "request_id", "candidate")
        val candidate = envelope.getJSONObject("candidate")
        keys(candidate, "state")
        check(string(candidate, "state") == "cleared")
        ReferralClearedResponse(requestId(envelope))
    }

    fun parseInfo(envelope: JSONObject): ReferralInfoResponse = strict {
        keys(envelope, "request_id", "referral")
        val info = envelope.getJSONObject("referral")
        keys(info, "account_ref", "code", "links", "attribution", "benefits", "rewards", "terms_version")
        val links = info.getJSONObject("links")
        keys(links, "telegram", "web")
        val attribution = info.getJSONObject("attribution")
        keys(attribution, "state", "receipt_id", "reason")
        val state = string(attribution, "state").also { check(it in setOf("none", "attached", "rejected")) }
        val receiptId = nullableString(attribution, "receipt_id")?.also { check(UUID.matches(it)) }
        val reason = nullableString(attribution, "reason")
        when (state) {
            "none" -> check(receiptId == null && reason == null)
            "attached" -> check(receiptId != null && reason == null)
            "rejected" -> check(receiptId != null && reason in REJECTION_REASONS)
        }
        val benefits = info.getJSONObject("benefits")
        keys(benefits, "trial_bonus_days", "discount")
        val discount = benefits.getJSONObject("discount")
        keys(discount, "currency", "amount_minor", "state")
        val rewards = info.getJSONObject("rewards")
        keys(rewards, "waiting_days", "applied_days")
        val referralCode = code(info, "code")
        val telegram = string(links, "telegram").also {
            check(it == "https://t.me/terlimo_vpn_wdtt_bot?start=ref_u$referralCode")
        }
        val web = string(links, "web").also { check(it == "https://terlimo.xyz/?ref=u$referralCode") }
        ReferralInfoResponse(requestId(envelope), ReferralInfo(
            accountRef = string(info, "account_ref").also { check(UUID.matches(it)) }, code = referralCode,
            telegramLink = telegram, webLink = web,
            attribution = ReferralAttribution(state, receiptId, reason),
            trialBonusDays = nonNegativeLong(benefits, "trial_bonus_days"),
            discount = ReferralDiscount(string(discount, "currency").also { check(it == "RUB") },
                nonNegativeLong(discount, "amount_minor"), string(discount, "state").also {
                    check(it in setOf("eligible", "reserved", "consumed", "ineligible", "history_pending"))
                }),
            waitingDays = nonNegativeLong(rewards, "waiting_days"),
            appliedDays = nonNegativeLong(rewards, "applied_days"), termsVersion = string(info, "terms_version").also {
                check(it == TERMS_VERSION)
            },
        ))
    }

    /** Unknown server/native codes stay unavailable; an error never means attribution succeeded. */
    fun errorText(code: String): String = when (code) {
        "REFERRAL_CODE_INVALID", "BAD_MESSAGE" -> "Код приглашения не принят. Проверьте код."
        "REFERRAL_CANDIDATE_LOCKED" -> "Код закреплён за текущей регистрацией. Дождитесь её результата."
        "REFERRAL_HISTORY_PENDING" -> "История приглашений ещё сверяется. Данные пока недоступны."
        "IDEMPOTENCY_CONFLICT" -> "Операция не согласована с сохранённой попыткой. Результат не подтверждён."
        "SESSION_INVALID", "SESSION_EXPIRED", "TELEGRAM_REQUIRED", "ACCESS_DENIED", "DEVICE_REVOKED" ->
            "Для просмотра нужен подтверждённый текущий аккаунт."
        "REGISTRATION_EXPIRED" -> "Ссылка регистрации истекла. Можно явно запросить новую с тем же кодом приглашения."
        else -> "Данные приглашения пока недоступны. Результат не подтверждён."
    }

    private fun code(obj: JSONObject, key: String) = string(obj, key).also { check(validCode(it)) }
    private fun requestId(obj: JSONObject): String = string(obj, "request_id").also { check(REQUEST_ID.matches(it)) }
    private fun utc(obj: JSONObject, key: String): String = string(obj, key).also { check(validUtcTime(it)) }
    private fun nonNegativeLong(obj: JSONObject, key: String): Long {
        val value = obj.get(key)
        check(value is Int || value is Long)
        return (value as Number).toLong().also { check(it >= 0) }
    }
    private fun string(obj: JSONObject, key: String, limit: Int = 512): String {
        val value = obj.get(key)
        check(value is String && value.length in 1..limit && value.none { it.code < 32 || it.code == 127 })
        return value
    }
    private fun nullableString(obj: JSONObject, key: String): String? =
        if (obj.get(key) === JSONObject.NULL) null else string(obj, key)
    private fun keys(obj: JSONObject, vararg expected: String) {
        check(obj.keys().asSequence().toSet() == expected.toSet())
    }
    private inline fun <T> strict(read: () -> T): T = try { read() } catch (e: Exception) {
        throw IllegalStateException("REFERRAL_INVALID", e)
    }
}

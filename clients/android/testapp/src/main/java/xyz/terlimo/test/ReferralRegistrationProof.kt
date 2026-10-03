package xyz.terlimo.test

import org.json.JSONObject

/**
 * Checks the subject proof in a keyed native registration result. Native validates the full
 * GET /me DTO; this helper neither projects its access fields nor grants account/data access.
 * Original installation, flight, K, candidate and disk CAS still belong to the caller.
 */
internal object ReferralRegistrationProof {
    private val verifiedStates = setOf(
        "VERIFIED_NO_ENTITLEMENT", "VERIFIED_NO_SLOT", "ACTIVE_TRIAL", "ACTIVE_PAID", "EXPIRED",
    )

    fun freshAccount(event: JSONObject, expectedAccount: String?): String? {
        if (event.opt("state") != "registered") return null
        val me = event.opt("fresh_me") as? JSONObject ?: return null
        val requestId = me.opt("request_id") as? String ?: return null
        val serverTime = me.opt("server_time") as? String ?: return null
        val accountState = me.opt("account_state") as? String ?: return null
        if (!ReferralContract.validRequestId(requestId) || !ReferralContract.validUtcTime(serverTime) ||
            me.opt("status") != "ok" || me.opt("schema_version") != "1.0" ||
            me.opt("telegram_linked") != true || accountState !in verifiedStates) return null
        val account = me.opt("account_ref") as? String ?: return null
        val eventAccount = event.opt("account_ref") as? String ?: return null
        if (!ReferralContract.validUuid(account) || account != eventAccount ||
            (expectedAccount != null && account != expectedAccount)) return null
        return account
    }

    /** A status marker only; the complete expiry payload must still pass contract correlation. */
    fun expiryStatus(event: JSONObject): Int? {
        if (event.opt("state") != "error" || event.opt("code") != "REGISTRATION_EXPIRED" ||
            event.opt("referral_registration_expired") != true) return null
        return when (val status = event.opt("http_status")) {
            is Int -> status.takeIf { it == 410 }
            is Long -> if (status == 410L) 410 else null
            else -> null
        }
    }
}

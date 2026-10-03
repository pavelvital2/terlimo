package xyz.terlimo.test

/** Single explicit foreground request, fenced to its child stream and current account. */
internal data class ReferralFlight(
    val requestId: String, val operation: String, val key: String? = null,
    val expectedAccount: String? = null, val attempt: String? = null,
    val cold: Boolean = false, val dispatched: Boolean = false,
    val openRegistrationLink: Boolean = false,
)
internal class ReferralTransportGate {
    @Volatile var flight: ReferralFlight? = null
        private set
    @Synchronized fun begin(requestId: String, operation: String, key: String?, account: String?, attempt: String?,
        openRegistrationLink: Boolean = false): Boolean {
        if (flight != null) return false
        flight = ReferralFlight(requestId, operation, key, account, attempt, cold = attempt == null, openRegistrationLink = openRegistrationLink)
        return true
    }
    @Synchronized fun started(attempt: String): Boolean {
        val f = flight ?: return false
        if (!f.cold || f.attempt != null) return false
        flight = f.copy(attempt = attempt)
        return true
    }
    @Synchronized fun dispatch(attempt: String, freshAccount: String?): ReferralFlight? {
        val f = flight ?: return null
        if (f.attempt != attempt || f.dispatched) return null
        if (f.expectedAccount != null && f.expectedAccount != freshAccount) return null
        if (f.operation == "info" && freshAccount == null) return null
        return f.copy(dispatched = true, expectedAccount = f.expectedAccount ?: freshAccount).also { flight = it }
    }
    @Synchronized fun accepts(attempt: String, requestId: String, freshAccount: String?): Boolean {
        val f = flight ?: return false
        return f.dispatched && f.attempt == attempt && f.requestId == requestId &&
            f.expectedAccount == freshAccount
    }
    /** A registration may link a previously anonymous subject; full /me+receipt must prove it. */
    @Synchronized fun acceptsRegistration(attempt: String, requestId: String, currentAccount: String?, registered: Boolean): Boolean {
        val f = flight ?: return false
        if (f.operation != "registration" || !f.dispatched || f.attempt != attempt || f.requestId != requestId) return false
        return f.expectedAccount == currentAccount || (registered && f.expectedAccount == null)
    }

    /** A new foreground command takes over a service-only attempt before its result arrives. */
    @Synchronized fun relinquishCold() { flight = flight?.copy(cold = false) }
    @Synchronized fun finish(): ReferralFlight? = flight.also { flight = null }
}

/** A definitive error proof must retain its exact native JSON types, never getInt coercion. */
internal object ReferralBridgeProof {
    fun rejection(event: org.json.JSONObject, installationId: String, key: String): ReferralCandidateRejection? {
        if (event.opt("definitive_rejection") != true || event.opt("retryable") != false) return null
        val rawStatus = event.opt("http_status")
        if (rawStatus !is Int && rawStatus !is Long) return null
        val status = (rawStatus as Number).toLong()
        if (status != 400L && status != 404L) return null
        val requestId = event.opt("request_id") as? String ?: return null
        val code = event.opt("code") as? String ?: return null
        if (!ReferralContract.validRequestId(requestId)) return null
        if (!((status == 404L && code == "REFERRAL_CODE_INVALID") || (status == 400L && code == "BAD_MESSAGE"))) return null
        return ReferralCandidateRejection(installationId, key, requestId, status.toInt(), code)
    }
}

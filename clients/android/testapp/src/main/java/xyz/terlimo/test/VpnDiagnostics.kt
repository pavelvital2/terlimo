package xyz.terlimo.test

/** Closed VPN setup evidence only; never accepts native error text. */
internal object VpnDiagnostics {
    private val stages = setOf("PENDING", "HANDSHAKE", "AUTH", "CONFIG", "BRIDGE")
    private val errors = setOf("NONE", "TIMEOUT", "CANCELED", "FAILED")
    private val authCodes = setOf("NONE", "UNKNOWN", "AUTH_REQUIRED", "BAD_MESSAGE", "GRANT_REVOKED",
        "LEASE_CONFLICT", "CHALLENGE_EXPIRED", "LEASE_EXPIRED", "PROOF_INVALID", "TRUST_FAILED",
        "KEY_UNAVAILABLE", "TRANSPORT_CLOSED", "RETRY_EXHAUSTED")

    fun parse(stage: Any?, error: Any?, authCode: Any?): Map<String, String>? {
        if (stage !is String || stage !in stages || error !is String || error !in errors ||
            authCode !is String || authCode !in authCodes) return null
        return mapOf("vpn_stage" to stage, "vpn_error_class" to error, "vpn_auth_code" to authCode)
    }
}

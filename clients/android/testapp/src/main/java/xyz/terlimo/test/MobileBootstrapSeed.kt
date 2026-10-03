package xyz.terlimo.test

import org.json.JSONObject
import java.net.URL
import java.util.Base64

/**
 * Trusted packaged mobile-v1 bootstrap seed (`test-mobile.json`).
 *
 * The seed is the only authority that can admit an attempt without a legacy `.link`:
 * it proves that the native child is configured for the mobile PoP bootstrap instead
 * of the wlbs legacy link flow. An arbitrary URL from any other source never qualifies,
 * no pseudo-link is synthesized, and a saved legacy link is never substituted.
 *
 * Note: a valid seed proves the configured bootstrap input only; it is not proof that
 * the live VK bootstrap transport works.
 */
internal data class MobileBootstrapSeed(
    val baseUrl: String,
    val environment: String,
    /**
     * Optional public service-channel seed block (the raw `service` object of the
     * packaged asset). It stays opaque here: the native child parses and validates
     * the typed schema strictly, and an absent block keeps the accepted HTTPS path.
     */
    val serviceSeed: String? = null,
    /** Packaged public verification key. Native remains the authority for recovery validation. */
    val recoveryVerifyKeyB64: String? = null,
) {
    companion object {
        /** Contract environments accepted by the native mobile session. */
        private val ENVIRONMENTS = setOf("test", "production")

        /**
         * Returns the seed only when the packaged asset read succeeded and the seed is
         * validly configured: `base_url` parses as https with a non-empty host and no
         * userinfo, and the environment is one of the contract values. An absent/empty
         * environment resolves to the same "test" default the native session applies;
         * anything else (including an unreadable or malformed asset) is invalid.
         */
        fun parse(raw: String?): MobileBootstrapSeed? {
            if (raw == null) return null
            val seed = runCatching { JSONObject(raw) }.getOrNull() ?: return null
            val baseUrl = seed.optString("base_url")
            val parsed = runCatching { URL(baseUrl) }.getOrNull() ?: return null
            if (parsed.protocol != "https" || parsed.host.isEmpty() || parsed.userInfo != null) return null
            val environment = seed.optString("environment", "test").ifEmpty { "test" }
            if (environment !in ENVIRONMENTS) return null
            val serviceRaw = seed.opt("service")
            if (serviceRaw != null && serviceRaw !is JSONObject) return null
            val serviceSeed = (serviceRaw as? JSONObject)?.toString()
            // A malformed optional key disables recovery without invalidating ordinary bootstrap.
            val recoveryKey = (seed.opt("recovery_verify_key_b64") as? String)?.takeIf { key ->
                key.length == 43 && key.matches(Regex("^[A-Za-z0-9_-]+$")) &&
                    runCatching {
                        val bytes = Base64.getUrlDecoder().decode(key)
                        bytes.size == 32 && Base64.getUrlEncoder().withoutPadding().encodeToString(bytes) == key
                    }.getOrDefault(false)
            }
            return MobileBootstrapSeed(baseUrl, environment, serviceSeed, recoveryKey)
        }
    }
}

/**
 * Link-gate policy for `SessionService.begin`: a real link (explicit import or saved)
 * is always sufficient; an empty link is admitted only by a valid packaged mobile seed.
 */
internal object MobileBootstrapGate {
    fun admits(activeLink: String, seed: MobileBootstrapSeed?): Boolean =
        activeLink.isNotEmpty() || seed != null

    // A real imported or retained subscription keeps the established link path.
    // The packaged service seed is only the bootstrap for a linkless installation.
    fun forLink(activeLink: String, seed: MobileBootstrapSeed?): MobileBootstrapSeed? =
        if (activeLink.isEmpty()) seed else null
}

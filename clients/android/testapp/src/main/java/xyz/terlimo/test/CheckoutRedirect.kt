package xyz.terlimo.test

import java.net.URI

/**
 * §3.2B browser checkout opening. The provider-issued HTTPS URL carried by the accepted
 * server `checkout_reference` is opened with `ACTION_VIEW` only as the consequence of an
 * explicit user action (see [CheckoutOpenPolicy]). Only a well-formed HTTPS URL without
 * userinfo and within the accepted wire bound is ever opened; a missing/invalid/oversized
 * reference opens nothing and the order is kept for status check/retry.
 */
internal data class CheckoutOpen(val paymentId: String, val url: String)

internal object CheckoutRedirect {
    /** The accepted wire bound of `checkout_reference` (PaymentsContract: 256). */
    private const val MAX_URL = 256

    /** The provider URL only for a bounded, well-formed https URL without userinfo. */
    fun providerUrl(reference: String?): String? {
        val value = reference?.trim()?.takeIf { it.isNotEmpty() } ?: return null
        if (value.length > MAX_URL) return null
        val uri = runCatching { URI(value) }.getOrNull() ?: return null
        if (uri.scheme?.lowercase() != "https") return null
        if (uri.host.isNullOrBlank()) return null
        if (uri.userInfo != null) return null
        return value
    }
}

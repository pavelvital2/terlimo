package xyz.terlimo.test

import java.net.URI

/**
 * One bounded logcat line per mobile attempt: the public installation fingerprint (SHA-256
 * hex64, the same value sent on the start wire) and the normalized configured mobile endpoint
 * (scheme/host/port only). Paths, queries, userinfo, tokens and keys are never emitted.
 */
internal object MobileAttemptDiagnostics {
    const val TAG = "WDTT/Mobile"
    const val MAX_LINE = 256
    private val INSTALLATION_ID = Regex("^[0-9a-f]{64}$")

    fun line(installationId: String, baseUrl: String): String {
        val id = installationId.takeIf { INSTALLATION_ID.matches(it) } ?: "unavailable"
        val endpoint = MobileBaseUrl.normalize(baseUrl) ?: "invalid"
        return "mobile_attempt installation_id=$id endpoint=$endpoint"
    }
}

internal object MobileBaseUrl {
    private const val MAX_HOST = 128

    /**
     * Returns `scheme://host:port` with userinfo/path/query/fragment stripped, or null when the
     * value is not an http(s) URL with a bounded host and a valid port. Display-only.
     */
    fun normalize(raw: String): String? {
        val uri = runCatching { URI(raw) }.getOrNull() ?: return null
        val scheme = uri.scheme?.lowercase() ?: return null
        if (scheme != "http" && scheme != "https") return null
        val host = uri.host ?: return null
        if (host.isEmpty() || host.length > MAX_HOST) return null
        if (host.any { it.isWhitespace() || it in "/@?#" }) return null
        val port = if (uri.port == -1) (if (scheme == "https") 443 else 80) else uri.port
        if (port !in 1..65535) return null
        return "$scheme://${host.lowercase()}:$port"
    }
}

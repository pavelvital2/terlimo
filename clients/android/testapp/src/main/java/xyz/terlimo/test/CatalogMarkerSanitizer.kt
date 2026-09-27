package xyz.terlimo.test

/**
 * Strict, secret-safe formatting of one [CatalogTimerMarkerRecord] into the production
 * `WDTT/Catalog` line. Every appended value is allow-listed: fixed rights/clear reasons,
 * a decimal-bounded revision, and an attempt token restricted to hex/dash. Anything else
 * becomes the explicit fixed fallback `invalid`/`unknown`, so no arbitrary string, newline
 * or URL can ever enter the diagnostic line. Observation-only: no timer decision reads it.
 */
internal object CatalogMarkerSanitizer {
    private val REVISION = Regex("^[0-9]{1,19}$")
    private val REASONS = setOf("rights_none", "rights_restricted_checkout", "clear")
    private val ATTEMPT = Regex("^[0-9a-fA-F-]{0,8}$")

    fun reason(value: String?): String? = when {
        value == null -> null
        REASONS.contains(value) -> value
        else -> "invalid"
    }

    fun revision(value: String?): String? = when {
        value == null -> null
        REVISION.matches(value) -> value
        else -> "invalid"
    }

    fun attempt(value: String): String {
        val prefix = if (value.length >= 8) value.substring(0, 8) else value
        return if (ATTEMPT.matches(prefix)) prefix else "unknown"
    }

    fun line(marker: CatalogTimerMarkerRecord): String = buildString {
        append("code=catstage:").append(marker.marker.name).append(':').append(attempt(marker.attempt))
        append(":elapsed=").append(marker.elapsedMs)
        append(":utc=").append(marker.utcMs)
        reason(marker.reason)?.let { append(":reason=").append(it) }
        revision(marker.revision)?.let { append(":rev=").append(it) }
    }
}

package xyz.terlimo.test

/**
 * Bounded, secret-free terminal diagnostics.
 *
 * A native/host terminal code can be collapsed to `HOST_ERROR` for the user by
 * [UserStatusText]. This formatter records the otherwise-lost stage so a failure stays
 * observable without ever echoing arbitrary input.
 *
 * It records only four fixed, validated fields: an internal `boundary`, the current
 * `phase`, the bridge `attempt` (UUID shape), a sanitized safe `code`, and an exception
 * class name. Anything not matching the strict patterns below is replaced by a fixed
 * fallback, so links, tokens, keys, payloads, headers or raw response text can never be
 * written to the log.
 */
internal object FailureDiagnostics {
    private val BOUNDARY = Regex("[a-z][a-z0-9_]{0,31}")
    private val PHASE = Regex("[A-Za-z][A-Za-z0-9_]{0,31}")
    private val ATTEMPT = Regex("[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
    private val CODE = Regex("[A-Z][A-Z0-9_]{1,63}")
    private val ERROR_CLASS = Regex("[A-Za-z][A-Za-z0-9_$.]{0,63}")

    /** Never echoes arbitrary input; unknown shapes fall back to a fixed token. */
    fun line(boundary: String, phase: String, attempt: String?, code: String?, errorClass: String?): String {
        val safeBoundary = if (BOUNDARY.matches(boundary)) boundary else "-"
        val safePhase = if (PHASE.matches(phase)) phase else "-"
        val safeAttempt = if (attempt != null && ATTEMPT.matches(attempt)) attempt else "-"
        val safeCode = when {
            code == null -> "-"
            CODE.matches(code) -> code
            else -> "HOST_ERROR"
        }
        val safeClass = if (errorClass != null && ERROR_CLASS.matches(errorClass)) errorClass else "-"
        return "boundary=$safeBoundary phase=$safePhase attempt=$safeAttempt code=$safeCode exception=$safeClass"
    }
}

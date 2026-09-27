package xyz.terlimo.test

/**
 * Fixed, secret-free formatter for one readiness failure line. It prints only the frozen
 * readiness vocabulary (stage/exceptionClass/failureCode), the boolean facts and bounded
 * numbers. Arbitrary strings (URLs, DNS answers, response bodies, exception text) are never
 * echoed; unknown shapes fall back to a fixed token.
 */
internal object ReadinessDiagnostics {
    private val STAGES = setOf("APPLY", "VPN_NETWORK", "DNS", "HTTPS", "HTTP_STATUS", "EXPECTED_EXIT", "WG_STATS", "READY")
    private val EXCEPTIONS = setOf("NONE", "TIMEOUT", "DNS", "TLS", "IO", "SECURITY", "OTHER")
    private val CODE = Regex("[A-Z][A-Z0-9_]{1,63}")

    fun line(
        stage: String,
        exceptionClass: String,
        failureCode: String,
        dnsOk: Boolean,
        httpsOk: Boolean,
        httpStatusOk: Boolean,
        expectedExitOk: Boolean,
        elapsedMs: Long,
        deadline: Boolean,
    ): String {
        val safeStage = if (stage in STAGES) stage else "-"
        val safeException = if (exceptionClass in EXCEPTIONS) exceptionClass else "-"
        val safeCode = if (CODE.matches(failureCode)) failureCode else "UNKNOWN"
        val safeElapsed = if (elapsedMs in 0..600_000) elapsedMs else -1L
        return "stage=$safeStage exception=$safeException failure=$safeCode " +
            "dnsOk=$dnsOk httpsOk=$httpsOk httpStatusOk=$httpStatusOk expectedExitOk=$expectedExitOk " +
            "elapsed_ms=$safeElapsed deadline=$deadline"
    }
}

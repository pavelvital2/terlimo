package xyz.terlimo.test

/** Confirmed public entries; a homepage is not a recovery issuance endpoint. */
internal object FirstReleaseLinks {
    const val BOT_URL = HelpContent.BOT_URL
    const val SITE_URL = HelpContent.SITE_URL
    const val RECOVERY_ENTRY_TEXT =
        "Получите код восстановления через Telegram или сайт, затем вернитесь в приложение " +
            "и вставьте его в окно восстановления. Покупка и повторная регистрация не нужны."

    /** Deployment values are independent of account and backend availability. */
    fun recoveryBotUrl(
        confirmedUrl: String? = BuildConfig.RECOVERY_BOT_URL,
        environment: String = BuildConfig.RECOVERY_ENVIRONMENT,
    ): String? = recoveryUrl(confirmedUrl, when (environment) {
        "test" -> "https://t.me/terlimo_test_bot?start=recovery"
        "prod" -> "https://t.me/terlimo_vpn_wdtt_bot?start=recovery"
        else -> null
    })

    fun recoverySiteUrl(
        confirmedUrl: String? = BuildConfig.RECOVERY_SITE_URL,
        environment: String = BuildConfig.RECOVERY_ENVIRONMENT,
    ): String? = recoveryUrl(confirmedUrl, when (environment) {
        "test" -> "https://step036.193-5-251-217.sslip.io/api/public/recovery"
        "prod" -> "https://terlimo.xyz/api/public/recovery"
        else -> null
    })

    private fun recoveryUrl(confirmedUrl: String?, expected: String?): String? =
        confirmedUrl?.takeIf { expected != null && it == expected }
}

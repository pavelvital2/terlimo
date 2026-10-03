package xyz.terlimo.test

import java.net.URI

/** Confirmed public entries; a homepage is not a recovery issuance endpoint. */
internal object FirstReleaseLinks {
    const val BOT_URL = HelpContent.BOT_URL
    const val SITE_URL = HelpContent.SITE_URL
    const val RECOVERY_UNAVAILABLE_TEXT =
        "Получение кода восстановления через бот и сайт пока недоступно: " +
            "ссылки выдачи ещё не подключены. Покупка и повторная регистрация для восстановления не нужны."

    /** Future wiring supplies the confirmed complete URL; null never creates a guessed route. */
    fun recoveryBotUrl(confirmedUrl: String? = null): String? =
        recoveryUrl(confirmedUrl, BOT_URL, samePath = true)

    fun recoverySiteUrl(confirmedUrl: String? = null): String? =
        recoveryUrl(confirmedUrl, SITE_URL, samePath = false)

    private fun recoveryUrl(confirmedUrl: String?, officialEntry: String, samePath: Boolean): String? {
        val value = confirmedUrl?.takeIf { it.isNotBlank() } ?: return null
        val route = runCatching { URI(value) }.getOrNull() ?: return null
        val entry = URI(officialEntry)
        if (route.scheme != "https" || route.host != entry.host || route.port != entry.port ||
            route.rawUserInfo != null || route.rawFragment != null) return null
        if (samePath && route.rawPath != entry.rawPath) return null
        // The confirmed bot needs its issuance action, and the site needs a path or action.
        if (route.rawPath == entry.rawPath && route.rawQuery.isNullOrBlank()) return null
        return value
    }
}

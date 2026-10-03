package xyz.terlimo.test

import android.content.Context

/**
 * §27/07.6 static help content. Addresses are the confirmed public TERLIMO contacts
 * (root docs/commercial/TERLIMO_PUBLIC_SUPPORT_KB_SOURCE.md §11, reconfirmed 2026-09-30);
 * they are opened with the system browser/Telegram via ACTION_VIEW and never persisted.
 * Legal pages are opened as-is; their text is not reproduced in the app.
 */
internal object HelpContent {
    const val BOT_URL = "https://t.me/terlimo_vpn_wdtt_bot"
    const val SUPPORT_URL = "https://t.me/terlimo_vpn"
    const val SITE_URL = "https://terlimo.xyz/"
    const val PRIVACY_URL = "https://terlimo.xyz/privacy"
    const val AGREEMENT_URL = "https://terlimo.xyz/agreement"
    const val CHANNEL_URL = "https://t.me/Terlimo_VPN_channel"
    const val SUPPORT_GROUP_URL = "https://t.me/Terlimo_support"

    const val CONTACTS_TITLE = "Контакты и документы"
    const val BOT_LABEL = "Бот TERLIMO"
    const val SUPPORT_LABEL = "Написать в поддержку"
    const val CHANNEL_LABEL = "Новостной канал"
    const val SUPPORT_GROUP_LABEL = "Группа поддержки и обсуждения"
    const val SITE_LABEL = "Сайт TERLIMO"
    const val PRIVACY_LABEL = "Политика конфиденциальности"
    const val AGREEMENT_LABEL = "Условия использования"

    const val CONNECT_TITLE = "Подключение и срок доступа"
    const val CONNECT_TEXT =
        "Выберите доступный сервер и нажмите кнопку подключения. Когда бесплатный час или срок " +
            "подписки заканчивается, продлите подписку или оформите доступ. Новый бесплатный час " +
            "автоматически не выдаётся."

    const val SPEED_TITLE = "Качество соединения"
    const val SPEED_TEXT =
        "Строка «Готовые каналы: n из m» показывает, сколько каналов готово к работе из целевых. " +
            "Это не скорость в Мбит/с. Если каналов мало, обновите каталог и выберите доступный сервер вручную."

    const val NETWORK_TITLE = "Сеть и обновление каталога"
    const val NETWORK_TEXT =
        "Проверьте подключение к интернету и повторите обновление («Обновить каталог»). Если сервер " +
            "недоступен, выберите доступный шлюз вручную. Если ошибка сохраняется, напишите в поддержку " +
            "и приложите безопасную диагностику. Для восстановления подключения вставьте код бота " +
            "в разделе «Настройки» → «Восстановить подключение». Аккаунт и подписка сохраняются."

    /** Runtime version; never a hardcoded string. */
    fun versionText(context: Context): String {
        val info = runCatching {
            context.packageManager.getPackageInfo(context.packageName, 0)
        }.getOrNull()
        val name = info?.versionName
        val code = if (info == null) 0L else
            if (android.os.Build.VERSION.SDK_INT >= 28) info.longVersionCode else info.versionCode.toLong()
        return formatVersion(name, code)
    }

    fun formatVersion(name: String?, code: Long): String {
        val safeName = name?.takeIf { it.isNotBlank() } ?: "—"
        return if (code <= 0L) "Версия приложения: $safeName" else "Версия приложения: $safeName ($code)"
    }
}

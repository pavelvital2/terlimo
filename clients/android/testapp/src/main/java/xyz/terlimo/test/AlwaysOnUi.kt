package xyz.terlimo.test

import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.provider.Settings

/** Read platform state; never write secure settings or alter split/lockdown policy. */
internal object AlwaysOnUi {
    fun managed(): Boolean = SafeGoBackend.alwaysOnMode() == true
    fun status(): String = when (SafeGoBackend.alwaysOnMode()) {
        true -> "Постоянный VPN включён. Управление — в настройках Android. " + when (SafeGoBackend.lockdownMode()) {
            true -> "Блокировка без VPN включена: исключённые приложения также могут быть без доступа к сети."
            false -> "Блокировка подключений без VPN выключена."
            null -> "Состояние блокировки смотрите в настройках Android."
        }
        false -> "Постоянный VPN выключен. Его можно включить в настройках Android."
        null -> "Состояние постоянного VPN смотрите в настройках Android."
    } + " Подключение после перезагрузки возможно после разблокировки телефона."

    fun actionLabel(): String = if (managed()) "Настройки VPN" else "Отключить"
    fun action(context: Context): PendingIntent = if (managed()) {
        PendingIntent.getActivity(context, 2604, Intent(Settings.ACTION_VPN_SETTINGS),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
    } else PendingIntent.getService(context, 1, Intent(context, SessionService::class.java).setAction("cancel"),
        PendingIntent.FLAG_IMMUTABLE)
}

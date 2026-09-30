package xyz.terlimo.test

import android.provider.Settings

/**
 * §26.4 battery-optimization policy for the Settings surface.
 *
 * Android (and some OEM layers) may restrict TERLIMO's background work. The app only shows a
 * clear warning and a button that opens the platform battery screen; the user alone chooses
 * "Без ограничений / Не оптимизировать". Nothing is changed by the app and no permission is
 * requested. A single platform API can only prove THIS app's own exemption request state; it
 * never proves the absence of every OEM restriction, so the texts never claim a full PASS.
 */
internal enum class BatteryOptimizationState { EXEMPT, RESTRICTED, UNKNOWN }

internal object BatteryOptimizationPolicy {
    /** Honest warning while the platform reports an actual restriction for this package. */
    const val WARNING = "Android может ограничивать работу TERLIMO в фоне. " +
        "Откройте системные настройки батареи и выберите «Без ограничений / Не оптимизировать»."

    /** Cautious text when the state cannot be read; never a false "everything is fine". */
    const val UNKNOWN = "Не удалось проверить ограничения батареи на этом устройстве. " +
        "При проблемах с фоном откройте системные настройки батареи для TERLIMO."

    const val NO_WARNING = ""

    /** The one action button label; opens the platform settings where the user decides. */
    const val ACTION_TEXT = "Настроить"

    fun state(ignoringBatteryOptimizations: Boolean?): BatteryOptimizationState = when (ignoringBatteryOptimizations) {
        true -> BatteryOptimizationState.EXEMPT
        false -> BatteryOptimizationState.RESTRICTED
        null -> BatteryOptimizationState.UNKNOWN
    }

    /** EXEMPT hides the block (the warning disappears after the user lifts the restriction). */
    fun warningVisible(state: BatteryOptimizationState): Boolean =
        state != BatteryOptimizationState.EXEMPT

    fun text(state: BatteryOptimizationState): String = when (state) {
        BatteryOptimizationState.RESTRICTED -> WARNING
        BatteryOptimizationState.UNKNOWN -> UNKNOWN
        BatteryOptimizationState.EXEMPT -> NO_WARNING
    }

    /**
     * Platform screens in order: the app's own battery-optimization exemption screen first,
     * then the app details page and the generic settings as safe fallbacks. Only standard
     * system intents are used; no vendor-specific activity is hard-coded.
     */
    fun actions(): List<String> = listOf(
        Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS,
        Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
        Settings.ACTION_SETTINGS,
    )
}

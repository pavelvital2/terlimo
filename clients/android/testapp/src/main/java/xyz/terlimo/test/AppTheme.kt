package xyz.terlimo.test

import android.content.Context
import android.content.SharedPreferences
import android.content.res.Configuration

/** §26.1 theme modes offered in Settings; SYSTEM is the default when nothing is chosen. */
internal enum class ThemeMode { SYSTEM, LIGHT, DARK }

/**
 * Process-wide resolved theme. Each user-facing Activity sets it from the stored per-user
 * preference in attachBaseContext BEFORE any view is created, so the shared brand tokens
 * resolve to the right palette for that screen.
 */
internal object ThemeState {
    @Volatile var isDark: Boolean = true
}

/**
 * §26.1 per-user app theme preference and resolution.
 *
 * - stored in the app's private per-user SharedPreferences ("terlimo-theme"/"theme_mode");
 * - absent/invalid value means SYSTEM (default);
 * - SYSTEM follows the current uiMode configuration; explicit LIGHT/DARK ignore it;
 * - no new permission, no system-wide change (the owner's system theme is never touched).
 */
internal object AppTheme {
    const val PREFS = "terlimo-theme"
    const val KEY = "theme_mode"

    fun decode(raw: String?): ThemeMode = when (raw) {
        "light" -> ThemeMode.LIGHT
        "dark" -> ThemeMode.DARK
        else -> ThemeMode.SYSTEM
    }

    fun encode(mode: ThemeMode): String = when (mode) {
        ThemeMode.LIGHT -> "light"
        ThemeMode.DARK -> "dark"
        ThemeMode.SYSTEM -> "system"
    }

    /** Pure resolution: SYSTEM follows [systemNight], explicit modes ignore it. */
    fun isDark(mode: ThemeMode, systemNight: Boolean): Boolean = when (mode) {
        ThemeMode.LIGHT -> false
        ThemeMode.DARK -> true
        ThemeMode.SYSTEM -> systemNight
    }

    fun systemNight(configuration: Configuration): Boolean =
        (configuration.uiMode and Configuration.UI_MODE_NIGHT_MASK) == Configuration.UI_MODE_NIGHT_YES

    private fun prefs(context: Context): SharedPreferences =
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    fun load(context: Context): ThemeMode = decode(runCatching { prefs(context).getString(KEY, null) }.getOrNull())

    fun save(context: Context, mode: ThemeMode) {
        prefs(context).edit().putString(KEY, encode(mode)).apply()
    }

    /**
     * Applies the stored preference to a Context: records the resolved mode for the tokens and
     * overrides uiMode for explicit LIGHT/DARK so platform widgets and dialogs follow too.
     */
    fun wrap(base: Context): Context {
        val mode = load(base)
        val night = systemNight(base.resources.configuration)
        ThemeState.isDark = isDark(mode, night)
        if (mode == ThemeMode.SYSTEM) return base
        val configuration = Configuration(base.resources.configuration)
        configuration.uiMode = (configuration.uiMode and Configuration.UI_MODE_NIGHT_MASK.inv()) or
            if (ThemeState.isDark) Configuration.UI_MODE_NIGHT_YES else Configuration.UI_MODE_NIGHT_NO
        return base.createConfigurationContext(configuration)
    }

    /** Platform theme id matching the resolved mode; applied before setContentView. */
    fun platformTheme(): Int =
        if (ThemeState.isDark) android.R.style.Theme_Material_NoActionBar
        else android.R.style.Theme_Material_Light_NoActionBar
}

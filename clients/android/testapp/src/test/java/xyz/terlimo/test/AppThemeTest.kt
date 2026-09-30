package xyz.terlimo.test

import android.content.res.Configuration
import java.io.File
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class AppThemeTest {
    private fun source(name: String) = listOf(
        File("src/main/java/xyz/terlimo/test/$name"),
        File("testapp/src/main/java/xyz/terlimo/test/$name"),
    ).first { it.isFile }.readText()

    private fun configuration(night: Boolean) = Configuration().apply {
        uiMode = (uiMode and Configuration.UI_MODE_NIGHT_MASK.inv()) or
            if (night) Configuration.UI_MODE_NIGHT_YES else Configuration.UI_MODE_NIGHT_NO
    }

    @Test fun preferenceDecodesWithSystemAsTheDefault() {
        assertEquals(ThemeMode.SYSTEM, AppTheme.decode(null))
        assertEquals(ThemeMode.SYSTEM, AppTheme.decode("system"))
        assertEquals(ThemeMode.SYSTEM, AppTheme.decode("weird"))
        assertEquals(ThemeMode.LIGHT, AppTheme.decode("light"))
        assertEquals(ThemeMode.DARK, AppTheme.decode("dark"))
        // Round-trip of the stored encoding.
        ThemeMode.values().forEach { assertEquals(it, AppTheme.decode(AppTheme.encode(it))) }
    }

    @Test fun explicitModesIgnoreSystemNightWhileSystemFollowsIt() {
        assertTrue(AppTheme.isDark(ThemeMode.DARK, systemNight = false))
        assertTrue(AppTheme.isDark(ThemeMode.DARK, systemNight = true))
        assertFalse(AppTheme.isDark(ThemeMode.LIGHT, systemNight = true))
        assertFalse(AppTheme.isDark(ThemeMode.LIGHT, systemNight = false))
        assertTrue(AppTheme.isDark(ThemeMode.SYSTEM, systemNight = true))
        assertFalse(AppTheme.isDark(ThemeMode.SYSTEM, systemNight = false))
        assertTrue(AppTheme.systemNight(configuration(true)))
        assertFalse(AppTheme.systemNight(configuration(false)))
    }

    @Test fun brandTokensFollowTheResolvedThemeWithReadableContrast() {
        val previous = ThemeState.isDark
        try {
            ThemeState.isDark = true
            val darkBg = TerlimoCatalogBrandTokens.BACKGROUND
            val darkText = TerlimoCatalogBrandTokens.TEXT
            ThemeState.isDark = false
            val lightBg = TerlimoCatalogBrandTokens.BACKGROUND
            val lightText = TerlimoCatalogBrandTokens.TEXT
            assertNotEquals(darkBg, lightBg)
            assertNotEquals("dark text must contrast the dark background", darkBg, darkText)
            assertNotEquals("light text must contrast the light background", lightBg, lightText)
            // Light background is light; dark background is dark (luminance ordering).
            assertTrue("light bg should be bright", luminance(lightBg) > 0.6f)
            assertTrue("dark bg should be dark", luminance(darkBg) < 0.2f)
            assertTrue("dark text should be bright", luminance(darkText) > 0.6f)
            assertTrue("light text should be dark", luminance(lightText) < 0.3f)
        } finally {
            ThemeState.isDark = previous
        }
    }

    private fun luminance(color: Int): Float {
        val r = (color shr 16 and 0xFF) / 255f
        val g = (color shr 8 and 0xFF) / 255f
        val b = (color and 0xFF) / 255f
        return 0.2126f * r + 0.7152f * g + 0.0722f * b
    }

    @Test fun everyUserFacingActivityResolvesTheStoredTheme() {
        for (name in listOf("MainActivity.kt", "RetentionSettingsActivity.kt",
            "RoutingSettingsActivity.kt", "ManlCaptchaWebViewManager.kt", "QrCaptureActivity.kt")) {
            val src = source(name)
            assertTrue("$name missing attachBaseContext", src.contains("override fun attachBaseContext"))
            assertTrue("$name missing AppTheme.wrap", src.contains("super.attachBaseContext(AppTheme.wrap(newBase))"))
        }
        // The platform widget theme follows the resolved mode where the app owns the theme.
        val main = source("MainActivity.kt")
        assertTrue(main.contains("setTheme(AppTheme.platformTheme())"))
    }

    @Test fun settingsChoiceSavesAndRecreatesWithTabAndInputPreserved() {
        val main = source("MainActivity.kt")
        // Three explicit choices with the exact approved labels.
        listOf("\"Системная\"", "\"Светлая\"", "\"Тёмная\"").forEach {
            assertTrue("missing theme choice $it", main.contains(it))
        }
        val block = main.substringAfter("themeGroup.setOnCheckedChangeListener")
            .substringBefore("addView(themeGroup)")
        assertTrue(block.contains("AppTheme.save(this@MainActivity, mode)"))
        assertTrue(block.contains("recreate()"))
        // Recreate must not repeat service side effects and must keep the visible tab/input.
        assertTrue(main.contains("STATE_VISIBLE_TAB"))
        assertTrue(main.contains("STATE_LINK_TEXT"))
        val restore = main.substringAfter("val restoredState = BottomNavigation.restoreSelection")
            .substringBefore("restoreTarget = null")
        assertFalse("restore must not run side-effecting onTab", restore.contains("probe_all_cancel"))
        assertFalse(restore.contains("announcements_request"))
        assertFalse(restore.contains("RoutingSettingsActivity"))
    }
}

package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Source-level wiring checks for the five-tab shell (S5 §07.1) that cannot be driven
 * without the Android runtime: Routing must open the existing RoutingSettingsActivity,
 * Settings must be its own surface whose live entry opens RetentionSettingsActivity, and
 * the previous retention path inside the routing screen must be preserved.
 */
class FiveTabSettingsSourceTest {
    private fun source(path: String): String =
        listOf(File(path), File("testapp/$path"), File("../$path")).first { it.isFile }.readText()

    private val main = source("src/main/java/xyz/terlimo/test/MainActivity.kt")

    @Test
    fun `destinations are rendered from the shared five-tab model`() {
        assertTrue(main.contains("BottomNavigation.destinations.forEach"))
        assertTrue(main.contains("RoutingSettingsActivity"))
        assertTrue(main.contains("BottomNavigation.onTap("))
    }

    @Test
    fun `settings surface is a real view with the retention entry`() {
        assertTrue(main.contains("addView(settingsSurface,"))
        assertTrue(main.contains("\"Работа с выключенным экраном\""))
        assertTrue(main.contains("RetentionSettingsActivity::class.java"))
        // The settings surface toggles visibility like the other in-place tabs (no new Activity).
        assertTrue(main.contains("val surfaces = listOf(mainSurface, subscriptionSurface, settingsSurface, helpSurface)"))
    }

    @Test
    fun `routing screen keeps its existing retention entry`() {
        val routing = source("src/main/java/xyz/terlimo/test/RoutingSettingsActivity.kt")
        assertTrue(routing.contains("RetentionSettingsActivity::class.java"))
    }
}

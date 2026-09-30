package xyz.terlimo.test

import android.provider.Settings
import java.io.File
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class BatteryOptimizationPolicyTest {
    private val src = listOf(
        File("src/main/java/xyz/terlimo/test/MainActivity.kt"),
        File("testapp/src/main/java/xyz/terlimo/test/MainActivity.kt"),
    ).first { it.isFile }.readText()

    @Test fun stateMappingIsExplicitAndUnknownIsNotAPass() {
        assertEquals(BatteryOptimizationState.EXEMPT, BatteryOptimizationPolicy.state(true))
        assertEquals(BatteryOptimizationState.RESTRICTED, BatteryOptimizationPolicy.state(false))
        assertEquals(BatteryOptimizationState.UNKNOWN, BatteryOptimizationPolicy.state(null))
    }

    @Test fun warningVisibilityAndTextsFollowTheState() {
        assertFalse(BatteryOptimizationPolicy.warningVisible(BatteryOptimizationState.EXEMPT))
        assertTrue(BatteryOptimizationPolicy.warningVisible(BatteryOptimizationState.RESTRICTED))
        assertTrue(BatteryOptimizationPolicy.warningVisible(BatteryOptimizationState.UNKNOWN))
        assertEquals(BatteryOptimizationPolicy.WARNING,
            BatteryOptimizationPolicy.text(BatteryOptimizationState.RESTRICTED))
        assertEquals(BatteryOptimizationPolicy.UNKNOWN,
            BatteryOptimizationPolicy.text(BatteryOptimizationState.UNKNOWN))
        assertEquals("", BatteryOptimizationPolicy.text(BatteryOptimizationState.EXEMPT))
        // The texts must not promise a full OEM-clean PASS.
        assertFalse(BatteryOptimizationPolicy.WARNING.contains("полностью"))
        assertFalse(BatteryOptimizationPolicy.UNKNOWN.contains("всё в порядке"))
    }

    @Test fun systemActionsPreferTheAppExemptionScreenWithSafeFallbacks() {
        val actions = BatteryOptimizationPolicy.actions()
        assertEquals(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS, actions.first())
        assertTrue(actions.contains(Settings.ACTION_APPLICATION_DETAILS_SETTINGS))
        assertTrue(actions.contains(Settings.ACTION_SETTINGS))
    }

    @Test fun activityWiresPlatformCheckFallbackAndResumeRefresh() {
        assertTrue(src.contains("isIgnoringBatteryOptimizations(packageName)"))
        assertTrue(src.contains("as? PowerManager"))
        // ANFE/SecurityException fallback for the system intent, like §26.3.
        val open = src.substringAfter("private fun openBatteryOptimizationSettings()")
            .substringBefore("private fun openSystemVpnSettings()")
        assertTrue(open.contains("ActivityNotFoundException"))
        assertTrue(open.contains("SecurityException"))
        assertTrue(open.contains("ACTION_APPLICATION_DETAILS_SETTINGS"))
        // The warning is re-rendered on return from system settings.
        val resume = src.substringAfter("override fun onResume()").substringBefore("onActivityResult")
        assertTrue(resume.contains("renderBatteryOptimization()"))
        // No new permission is requested for the exemption screen.
        assertFalse(src.contains("REQUEST_IGNORE_BATTERY_OPTIMIZATIONS"))
    }
}

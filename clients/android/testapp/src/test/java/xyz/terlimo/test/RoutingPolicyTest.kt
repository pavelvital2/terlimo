package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class RoutingPolicyTest {
    private val presets = QuickExclusionCatalog(listOf(
        QuickExclusionPreset("banking", 1, setOf("com.example.bank", "com.example.pay")),
    ))
    private val protected = setOf("xyz.terlimo.test", "xyz.terlimo.bridge")

    @Test fun excludeAddsQuickPackagesDeterministicallyButNeverProtected() {
        val result = RoutingPolicyNormalizer.apps(AppRoutingMode.EXCLUDE, setOf("org.example.video"), setOf("banking"), protected, presets)
        assertEquals(setOf("com.example.bank", "com.example.pay", "org.example.video"), result.packageNames)
        assertFalse(result.packageNames.any { it in protected })
    }

    @Test fun includeIsExplicitAndRejectsQuickButStripsLegacyProtected() {
        val result = RoutingPolicyNormalizer.apps(AppRoutingMode.INCLUDE_ONLY, emptySet(), emptySet(), protected, presets)
        assertTrue(result.packageNames.isEmpty()) // explicitly route no applications
        assertThrows(IllegalArgumentException::class.java) { RoutingPolicyNormalizer.apps(AppRoutingMode.INCLUDE_ONLY, emptySet(), setOf("banking"), protected, presets) }
        // A stale legacy self entry is stripped, not rejected, so existing user choice survives.
        val legacy = RoutingPolicyNormalizer.apps(AppRoutingMode.INCLUDE_ONLY, setOf("xyz.terlimo.test", "org.example.app"), emptySet(), protected, presets)
        assertEquals(setOf("org.example.app"), legacy.packageNames)
    }

    @Test fun legacySelfEntryIsStrippedFromEveryMode() {
        assertEquals(emptySet<String>(),
            RoutingPolicyNormalizer.apps(AppRoutingMode.DISABLED, setOf("xyz.terlimo.test"), emptySet(), protected, presets).packageNames)
        assertEquals(setOf("org.example.video"),
            RoutingPolicyNormalizer.apps(AppRoutingMode.EXCLUDE, setOf("xyz.terlimo.test", "org.example.video"), emptySet(), protected, presets).packageNames)
    }

    @Test fun disabledIsExactAndRejectsHiddenRules() {
        assertEquals(AppRoutingMode.DISABLED,
            RoutingPolicyNormalizer.apps(AppRoutingMode.DISABLED, emptySet(), emptySet(), protected, presets).mode)
        assertThrows(IllegalArgumentException::class.java) {
            RoutingPolicyNormalizer.apps(AppRoutingMode.DISABLED, setOf("org.example.video"), emptySet(), protected, presets)
        }
    }

    @Test fun malformedPackagesAndUnknownPresetReject() {
        listOf("app", " bad.app", "bad/app", "*.example.app", "пример.app").forEach { invalid ->
            assertThrows(invalid, IllegalArgumentException::class.java) { RoutingPolicyNormalizer.apps(AppRoutingMode.EXCLUDE, setOf(invalid), emptySet(), protected, presets) }
        }
        assertThrows(IllegalArgumentException::class.java) { RoutingPolicyNormalizer.apps(AppRoutingMode.EXCLUDE, emptySet(), setOf("missing"), protected, presets) }
        assertThrows(IllegalArgumentException::class.java) { RoutingPolicyNormalizer.apps(AppRoutingMode.EXCLUDE, listOf("com.example.app", "com.example.app"), emptySet(), protected, presets) }
    }

}

package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

/**
 * STEP02.1 self-UID correction: the semantic user selection and the effective OS
 * allow/disallow sets are different views. Self/protected packages may appear in a
 * stale legacy persisted selection but must never reach VpnService.Builder.
 */
class RoutingSelfUidTest {
    private val self = "xyz.terlimo.test"
    private val protected = setOf(self)
    private val installed = setOf(self, "org.example.app", "org.example.video")
    private val presets = QuickExclusionCatalog(listOf(QuickExclusionPreset("banking", 1, setOf("com.example.bank"))))

    private fun doc(mode: AppRoutingMode, semantic: Set<String>, quick: Set<String> = emptySet()) =
        RoutingSettingsDocument(revision = 1, routing = RoutingPolicy(AppRoutingPolicy(mode, semantic, quick)))

    private fun plan(mode: AppRoutingMode, semantic: Set<String>, quick: Set<String> = emptySet()) =
        RoutingRuntimePlanner.plan(doc(mode, semantic, quick), listOf("1.1.1.1"), installed, protected)

    @Test fun disabledLeavesAllAppsInVpnAndListsNoSelf() {
        val p = plan(AppRoutingMode.DISABLED, emptySet())
        assertTrue(p.includedApplications.isEmpty())
        assertTrue(p.excludedApplications.isEmpty())
        assertEquals(listOf("0.0.0.0/0"), p.ipv4Routes)
    }

    @Test fun excludeEffectiveDisallowContainsOnlyUserAppsNeverSelf() {
        val p = plan(AppRoutingMode.EXCLUDE, setOf("org.example.video"))
        assertEquals(listOf("org.example.video"), p.excludedApplications)
        assertFalse(p.excludedApplications.contains(self))
        assertTrue(p.includedApplications.isEmpty())
        assertEquals(listOf("0.0.0.0/0"), p.ipv4Routes)
    }

    @Test fun excludeDropsStaleLegacySelfEntry() {
        val p = plan(AppRoutingMode.EXCLUDE, setOf(self, "org.example.video"))
        assertEquals(listOf("org.example.video"), p.excludedApplications)
    }

    @Test fun includeOnlyAddsSelfToEffectiveAllowSet() {
        val p = plan(AppRoutingMode.INCLUDE_ONLY, setOf("org.example.app"))
        assertEquals(listOf("org.example.app", self), p.includedApplications)
        assertTrue(p.excludedApplications.isEmpty())
        assertEquals(listOf("0.0.0.0/0"), p.ipv4Routes)
    }

    @Test fun includeOnlyEmptySelectionStaysFailClosedEvenWithLegacySelf() {
        val p = plan(AppRoutingMode.INCLUDE_ONLY, setOf(self))
        assertTrue(p.includedApplications.isEmpty())
        assertTrue(p.ipv4Routes.isEmpty())
    }

    @Test fun emptyIncludeSelectionIsRejectedWhileNonEmptyIsAllowed() {
        assertFalse(RoutingSettingsApply.shouldPersist(AppRoutingMode.INCLUDE_ONLY, emptySet()))
        assertTrue(RoutingSettingsApply.shouldPersist(AppRoutingMode.INCLUDE_ONLY, setOf("org.example.app")))
        assertTrue(RoutingSettingsApply.shouldPersist(AppRoutingMode.EXCLUDE, emptySet()))
    }

    @Test fun semanticPolicyNeverCarriesSelfButEffectiveSetIsStillSelfFree() {
        val semantic = RoutingPolicyNormalizer.apps(AppRoutingMode.EXCLUDE,
            setOf("org.example.video"), emptySet(), protected, presets)
        assertFalse(semantic.packageNames.contains(self))
        // Legacy document: semantic field may still hold self, but the effective set must not.
        val legacy = doc(AppRoutingMode.EXCLUDE, setOf(self, "org.example.video"))
        assertTrue(legacy.routing.apps.packageNames.contains(self))
        val effective = RoutingRuntimePlanner.plan(legacy, listOf("1.1.1.1"), installed, protected)
        assertFalse(effective.excludedApplications.contains(self))
    }

    @Test fun legacyPersistedSelfEntryDecodesSelfFreeWithoutLosingUnrelatedFields() {
        val legacy = "version\t1\nrevision\t9\napp_mode\tEXCLUDE\napp\torg.example.video\napp\t$self\nquick\tbanking\ndns\tautomatic\n"
        val decoded = RoutingSettingsCodec.decode(legacy, protected, presets)
        assertEquals(9L, decoded.revision)
        assertFalse(decoded.routing.apps.packageNames.contains(self))
        assertEquals(setOf("com.example.bank", "org.example.video"), decoded.routing.apps.packageNames)
        assertEquals(setOf("banking"), decoded.routing.apps.quickExclusionIds)
        assertFalse(RoutingSettingsCodec.encode(decoded).contains("\t$self\n"))
    }

    @Test fun normalizingSelfOutPreservesRevisionQuickIdsAndUnrelatedApps() {
        val normalized = RoutingPolicyNormalizer.apps(AppRoutingMode.EXCLUDE,
            setOf(self, "org.example.video"), setOf("banking"), protected, presets)
        assertEquals(AppRoutingMode.EXCLUDE, normalized.mode)
        assertEquals(setOf("com.example.bank", "org.example.video"), normalized.packageNames)
        assertEquals(setOf("banking"), normalized.quickExclusionIds)
        assertEquals(5L, RoutingSettingsDocument(revision = 5, routing = RoutingPolicy(normalized)).revision)
    }
}

package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class RoutingSettingsDocumentTest {
    private val presets = QuickExclusionCatalog(listOf(QuickExclusionPreset("banking", 1, setOf("com.example.bank"))))
    private val protected = setOf("xyz.terlimo.test")

    @Test fun safeSummaryContainsCountsAndNoRuleValues() {
        val routing = RoutingPolicy(AppRoutingPolicy(AppRoutingMode.EXCLUDE, setOf("org.example.app")))
        val summary = RoutingSettingsDocument(1, 3, routing).safeSummary()
        assertTrue(summary.contains("revision=3"))
        assertFalse(summary.contains("org.example.app"))
        assertFalse(summary.contains("1.1.1.1"))
    }

    @Test fun versionAndRevisionAreBounded() {
        val routing = RoutingPolicy(AppRoutingPolicy(AppRoutingMode.DISABLED, emptySet()))
        assertThrows(IllegalArgumentException::class.java) { RoutingSettingsDocument(2, 0, routing) }
        assertThrows(IllegalArgumentException::class.java) { RoutingSettingsDocument(1, -1, routing) }
    }

    @Test fun deterministicPayloadRoundTripsThroughStrictValidation() {
        val routing = RoutingPolicy(RoutingPolicyNormalizer.apps(
            AppRoutingMode.EXCLUDE, setOf("org.example.video"), setOf("banking"), protected, presets))
        val original = RoutingSettingsDocument(revision = 7, routing = routing)
        val encoded = RoutingSettingsCodec.encode(original)
        assertEquals(encoded, RoutingSettingsCodec.encode(RoutingSettingsCodec.decode(encoded, protected, presets)))
        assertFalse(encoded.contains("whitelists://"))
    }

    @Test fun unknownFieldsManualDnsAndTamperedKindReject() {
        val routing = RoutingPolicy(RoutingPolicyNormalizer.apps(
            AppRoutingMode.DISABLED, emptySet(), emptySet(), protected, presets))
        val encoded = RoutingSettingsCodec.encode(RoutingSettingsDocument(revision = 0, routing = routing))
        assertThrows(IllegalArgumentException::class.java) { RoutingSettingsCodec.decode(encoded + "extra\tvalue\n", protected, presets) }
        assertThrows(IllegalArgumentException::class.java) { RoutingSettingsCodec.decode(encoded.replace("dns\tautomatic", "dns\t1.1.1.1"), protected, presets) }
        assertThrows(IllegalArgumentException::class.java) { RoutingSettingsCodec.decode(encoded.replace("app_mode\tDISABLED", "app_mode\tUNKNOWN"), protected, presets) }
    }
}

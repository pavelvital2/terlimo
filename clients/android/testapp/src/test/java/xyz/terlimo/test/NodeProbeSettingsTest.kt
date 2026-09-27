package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Test

class NodeProbeSettingsTest {
    private val trust = """{"node_id":"trusted-node"}"""

    @Test
    fun legacyProbeIsBoundOnlyToImmutableTrustNode() {
        val settings = NodeProbeSettings.parse(
            """{"probe_url":"https://probe.example/ip","expected_exit_ip":"192.0.2.10"}""",
            trust,
        )

        assertEquals("https://probe.example/ip", settings["trusted-node"]?.probeUrl)
        assertNull(settings["wrong-node"])
        assertNull(settings["second-node"])
        assertEquals(setOf("trusted-node"), settings.toNativeJson().keys().asSequence().toSet())
    }

    @Test
    fun explicitTwoNodeSettingsDoNotFallbackBetweenNodes() {
        val settings = NodeProbeSettings.parse(
            """{"nodes":{"a":{"probe_url":"https://a.example/ip","expected_exit_ip":"192.0.2.1"},"b":{"probe_url":"https://b.example/ip","expected_exit_ip":"2001:db8::2"}}}""",
            trust,
        )

        assertEquals("192.0.2.1", settings["a"]?.expectedExitIp)
        assertEquals("2001:db8::2", settings["b"]?.expectedExitIp)
        assertNull(settings["c"])
    }

    @Test
    fun rejectsWrongTypesUnsafeUrlsAndNonIpValues() {
        val invalid = listOf(
            """{"nodes":{"a":{"probe_url":7,"expected_exit_ip":"192.0.2.1"}}}""",
            """{"nodes":{"a":{"probe_url":"http://a.example/ip","expected_exit_ip":"192.0.2.1"}}}""",
            """{"nodes":{"a":{"probe_url":"https://user@a.example/ip","expected_exit_ip":"192.0.2.1"}}}""",
            """{"nodes":{"a":{"probe_url":"https://a.example/ip#part","expected_exit_ip":"192.0.2.1"}}}""",
            """{"nodes":{"a":{"probe_url":"https://a.example/ip","expected_exit_ip":"example.com"}}}""",
        )

        invalid.forEach { document ->
            assertThrows(IllegalStateException::class.java) { NodeProbeSettings.parse(document, trust) }
        }
    }

    @Test
    fun acceptsArbitraryGatewayCountWithoutTruncation() {
        val document = """{"nodes":{"a":{"probe_url":"https://a.example","expected_exit_ip":"192.0.2.1"},"b":{"probe_url":"https://b.example","expected_exit_ip":"192.0.2.2"},"c":{"probe_url":"https://c.example","expected_exit_ip":"192.0.2.3"}}}"""

        val settings = NodeProbeSettings.parse(document, trust)
        assertEquals(setOf("a", "b", "c"), settings.toNativeJson().keys().asSequence().toSet())
    }

    @Test
    fun rejectsBeyondTheTechnicalBound() {
        val entries = (1..1025).joinToString(",") { index ->
            """"n$index":{"probe_url":"https://example.com","expected_exit_ip":"192.0.2.1"}"""
        }
        assertThrows(IllegalStateException::class.java) {
            NodeProbeSettings.parse("""{"nodes":{$entries}}""", trust)
        }
    }
}

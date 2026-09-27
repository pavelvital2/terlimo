package xyz.terlimo.test

import java.io.File
import java.util.zip.ZipFile
import org.junit.Assert.*
import org.junit.Test

/** Run after assembleDebug: validate the actual APK, not a copied fixture. */
class PackagedNodeProbeTest {
    @Test
    fun packagedNodeMappingUsesProductionParserWithoutUnknownNodeFallback() {
        val apk = File("build/outputs/apk/debug/testapp-debug.apk")
        assertTrue("assembleDebug must produce the candidate first", apk.isFile)
        ZipFile(apk).use { zip ->
            fun asset(name: String): String = zip.getInputStream(
                requireNotNull(zip.getEntry("assets/$name")),
            ).bufferedReader().use { it.readText() }

            val settings = NodeProbeSettings.parse(
                asset("test-probe.json"), asset("test-public-trust.json"),
            )
            assertEquals(setOf("terlimo-test-193-5-251-217", "test2", "terlimo-035-node", "terlimo-036-node"),
                settings.toNativeJson().keys().asSequence().toSet())
            assertEquals(NodeProbe("https://api.ipify.org", "193.5.251.217"),
                settings["terlimo-test-193-5-251-217"])
            assertEquals(NodeProbe("https://api.ipify.org", "23.26.193.88"),
                settings["test2"])
            assertEquals(NodeProbe("https://api.ipify.org", "193.5.251.217"),
                settings["terlimo-035-node"])
            assertEquals(NodeProbe("https://api.ipify.org", "193.5.251.217"),
                settings["terlimo-036-node"])
            // Null is the existing production admission's PROBE_NOT_PROVISIONED gate.
            assertNull(settings["unprovisioned-node"])
            assertFalse(settings.toNativeJson().has("unprovisioned-node"))
        }
    }
}

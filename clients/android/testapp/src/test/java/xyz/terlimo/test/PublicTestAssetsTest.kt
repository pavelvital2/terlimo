package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import java.io.File
import java.security.KeyFactory
import java.security.MessageDigest
import java.security.interfaces.ECPublicKey
import java.security.spec.X509EncodedKeySpec
import java.net.URI

class PublicTestAssetsTest {
    private fun asset(name: String): ByteArray = listOf(File("src/main/assets/$name"), File("testapp/src/main/assets/$name"))
        .first { it.isFile }.readBytes()

    @Test fun immutablePublicMetadataHasExactlyRealTestIssuerAndProbe() {
        val raw = asset("test-public-trust.json")
        val sum = MessageDigest.getInstance("SHA-256").digest(raw).joinToString("") { "%02x".format(it) }
        assertEquals("d3a54153dbc936b503dcab55da34a159787069272ce23beb59e86612c9e4e94f", sum)
        val meta = JSONObject(raw.toString(Charsets.UTF_8))
        assertEquals("test", meta.getString("env")); assertEquals(12, meta.length())
        val issuers = JSONObject(asset("issuers.json").toString(Charsets.UTF_8))
        assertEquals(1, issuers.length())
        assertEquals(meta.getString("issuer_spki_b64url"), issuers.getString(meta.getString("issuer_kid")))
        val key = KeyFactory.getInstance("EC").generatePublic(X509EncodedKeySpec(SigningPolicy.decode(meta.getString("issuer_spki_b64url")))) as ECPublicKey
        assertEquals(256, key.params.curve.field.fieldSize); assertEquals(256, key.params.order.bitLength())
        assertEquals(32, SigningPolicy.decode(meta.getString("dtls_spki_sha256")).size)
        val probe = JSONObject(asset("test-probe.json").toString(Charsets.UTF_8))
        assertEquals(1, probe.length())
        val nodes = probe.getJSONObject("nodes")
        assertEquals(setOf("terlimo-test-193-5-251-217", "test2", "terlimo-035-node", "terlimo-036-node"),
            nodes.keys().asSequence().toSet())
        assertEquals("193.5.251.217", nodes.getJSONObject("terlimo-036-node").getString("expected_exit_ip"))
        val primary = nodes.getJSONObject(meta.getString("node_id"))
        assertEquals(meta.getString("probe_url"), primary.getString("probe_url"))
        assertEquals(meta.getString("expected_exit_ip"), primary.getString("expected_exit_ip"))
        val uri = URI(primary.getString("probe_url"))
        assertEquals("https", uri.scheme); assertNull(uri.rawUserInfo); assertNull(uri.rawFragment)
        assertEquals("api.ipify.org", uri.host)
        // Numeric parsing only: this test must never query DNS or contact the probe.
        val octets = primary.getString("expected_exit_ip").split(".")
        assertEquals(4, octets.size); assertTrue(octets.all { it.toIntOrNull()?.let { n -> n in 0..255 } == true })
        assertEquals(36, meta.getInt("max_workers")); assertEquals(56002, meta.getInt("wg_port"))
    }
}

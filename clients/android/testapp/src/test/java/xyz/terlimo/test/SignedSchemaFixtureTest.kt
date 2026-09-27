package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import java.security.KeyFactory
import java.security.MessageDigest
import java.security.Signature
import java.security.spec.X509EncodedKeySpec
import java.time.Instant

/** Native owns catalog/auth schema parsing; host owns raw T, DER and worker metadata. */
class SignedSchemaFixtureTest {
    private fun fixture(): JSONObject {
        val bytes = javaClass.getResourceAsStream("/wl_schema_signed_v1.json")!!.use { it.readBytes() }
        assertArrayEquals(hex("edacbc6b0653a4bf63727fd9d8347405ece083b5bb578eaa9ab0f211a870c45d"), sha256(bytes))
        return JSONObject(bytes.toString(Charsets.UTF_8))
    }

    @Test fun exactSignedVpnTranscriptAndDerVerifyWithoutDoubleHash() {
        val root = fixture()
        val signed = root.getJSONObject("signed_vpn")
        val transcript = hex(signed.getString("transcript_hex"))
        // Reconstruct T from the fixture bytes, not reserialized JSON.
        val expected = "WL-VPN-POP-1\u0000".toByteArray(Charsets.US_ASCII) +
            hex(signed.getString("synthetic_exporter_hex")) +
            SigningPolicy.decode(signed.getString("challenge_id")) +
            SigningPolicy.decode(signed.getString("nonce")) +
            sha256(SigningPolicy.decode(signed.getString("payload_b64")))
        assertArrayEquals(expected, transcript)
        SigningPolicy.validate("vpn", transcript)
        assertArrayEquals(hex(signed.getString("sha256_transcript_hex")), sha256(transcript))
        val proof = SigningPolicy.decode(signed.getString("proof_b64"))
        assertEquals(signed.getString("proof_b64"), root.getJSONObject("vpn_auth").getString("proof_b64"))
        assertEquals(0x30, proof[0].toInt()) // ASN.1 DER sequence, not raw r || s.
        assertEquals(proof.size - 2, proof[1].toInt())
        val key = KeyFactory.getInstance("EC").generatePublic(
            X509EncodedKeySpec(SigningPolicy.decode(signed.getString("public_key_spki"))))
        fun verify(input: ByteArray): Boolean = Signature.getInstance("SHA256withECDSA").run {
            initVerify(key); update(input); this.verify(proof)
        }
        assertTrue(verify(transcript))
        assertFalse(verify(sha256(transcript)))
        assertFalse(verify(transcript.copyOf().apply { this[lastIndex] = (this[lastIndex].toInt() xor 1).toByte() }))
    }

    @Test fun fixtureWorkerMetadataRejectsNoncanonicalDecimalOnlyAtHostBoundary() {
        val root = fixture()
        SigningPolicy.validateWorker("vpn", root.getJSONObject("vpn_payload").getString("worker_id"))
        val negatives = root.getJSONArray("negative_schema")
        var checked = 0
        for (index in 0 until negatives.length()) {
            val entry = negatives.getJSONObject(index)
            if (entry.getString("field") != "worker_id" || entry.getString("reason") != "noncanonical decimal") continue
            assertThrows(IllegalArgumentException::class.java) {
                SigningPolicy.validateWorker("vpn", entry.getString("value"))
            }
            checked++
        }
        assertEquals(5, checked)
        // Mode=getconf and max_workers authorization belong to native, not this lexical check.
        SigningPolicy.validateWorker("vpn", "1")
        SigningPolicy.validateWorker("vpn", "999999999")
        assertThrows(IllegalArgumentException::class.java) { SigningPolicy.validateWorker("vpn", "1000000000") }
    }

    @Test fun fixtureDeadlineUsesFixedServerTimeAndExpiresAtEquality() {
        val challenge = fixture().getJSONObject("vpn_challenge")
        val now = Instant.parse(challenge.getString("server_time")).toEpochMilli()
        val deadline = Instant.parse(challenge.getString("challenge_expires_at")).toEpochMilli()
        assertEquals(15_000L, deadline - now)
        val gate = AttemptGate(); gate.start("fixture")
        assertTrue(gate.admit("fixture", "before", deadline, now))
        assertTrue(gate.finish("fixture", "before"))
        assertFalse(gate.admit("fixture", "at", deadline, deadline))
        assertFalse(gate.admit("fixture", "after", deadline, deadline + 1))
    }

    private fun sha256(bytes: ByteArray) = MessageDigest.getInstance("SHA-256").digest(bytes)
    private fun hex(value: String) = value.chunked(2).map { it.toInt(16).toByte() }.toByteArray()
}

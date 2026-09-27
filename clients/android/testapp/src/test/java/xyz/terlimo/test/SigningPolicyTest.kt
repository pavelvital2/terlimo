package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test
import org.json.JSONObject
import java.security.KeyFactory
import java.security.MessageDigest
import java.security.Signature
import java.security.spec.X509EncodedKeySpec

class SigningPolicyTest {
    @Test fun canonicalVectorsVerifyWithoutDoubleHash() {
        val fixtures = listOf(
            "/wl_wire_vectors.json" to "c8157e92c9c62e6781a2c8cb48fa020bb3f43f81414ce0890c96a68968a442dd",
            "/wl_wire_vectors_v2.json" to "0852d628bce188d61da7c49c40ca21653909547579bc8f7225724c0dab8ec832"
        )
        for ((name, sha256) in fixtures) {
            val bytes = javaClass.getResourceAsStream(name)!!.use { it.readBytes() }
            assertArrayEquals(hex(sha256), MessageDigest.getInstance("SHA-256").digest(bytes))
            val root = JSONObject(bytes.toString(Charsets.UTF_8))
            val vectors = root.getJSONArray("vectors")
            for (index in 0 until vectors.length()) {
                val vector = vectors.getJSONObject(index)
                val transcript = hex(vector.getString("transcript_hex"))
                SigningPolicy.validate(vector.getString("kind"), transcript)
                assertArrayEquals(hex(vector.getString("sha256_transcript")), MessageDigest.getInstance("SHA-256").digest(transcript))
                val publicKey = KeyFactory.getInstance("EC").generatePublic(X509EncodedKeySpec(SigningPolicy.decode(vector.getString("public_key_spki"))))
                val proof = SigningPolicy.decode(vector.getString("proof_b64"))
                val verifier = Signature.getInstance("SHA256withECDSA")
                verifier.initVerify(publicKey); verifier.update(transcript)
                assertTrue(verifier.verify(proof))
                verifier.initVerify(publicKey); verifier.update(MessageDigest.getInstance("SHA-256").digest(transcript))
                assertFalse(verifier.verify(proof))
            }
            val link = root.getJSONObject("link")
            val publicKey = KeyFactory.getInstance("EC").generatePublic(X509EncodedKeySpec(SigningPolicy.decode(link.getString("public_key_spki"))))
            assertTrue(Signature.getInstance("SHA256withECDSA").run {
                initVerify(publicKey); update(hex(link.getString("transcript_hex"))); verify(SigningPolicy.decode(link.getString("proof_b64")))
            })
        }
    }
    @Test fun cancelAndOldAttemptRejectLateSignatures() {
        val gate = AttemptGate()
        gate.start("one")
        assertTrue(gate.admit("one", "signature", 15000, 0))
        assertFalse(gate.admit("one", "signature", 15000, 0))
        gate.cancel()
        assertFalse(gate.finish("one", "signature"))
        gate.start("two")
        assertFalse(gate.admit("one", "late", 15000, 0))
        assertFalse(gate.admit("two", "expired", 0, 1))
    }
    @Test fun boundedSigningQueue() {
        val gate = AttemptGate(); gate.start("one")
        repeat(4) { assertTrue(gate.admit("one", "$it", 15000, 0)) }
        assertFalse(gate.admit("one", "overflow", 15000, 0))
    }
    @Test fun fourPendingIncludesRunningAndCapacityIsReleasedOnce() {
        val gate = AttemptGate(); gate.start("one")
        assertTrue(gate.admit("one", "running", 15000, 0))
        repeat(3) { assertTrue(gate.admit("one", "queued-$it", 15000, 0)) }
        assertFalse(gate.admit("one", "fifth", 15000, 0))
        assertFalse(gate.finish("wrong-attempt", "running"))
        assertFalse(gate.finish("one", "not-pending"))
        assertFalse(gate.admit("one", "still-fifth", 15000, 0))
        assertTrue(gate.finish("one", "running"))
        assertFalse(gate.finish("one", "running"))
        assertTrue(gate.admit("one", "replacement", 15000, 0))
        assertFalse(gate.admit("one", "overflow", 15000, 0))
    }
    @Test fun cancelClearsFullQueueAndOldCompletionCannotConsumeNewRequest() {
        val gate = AttemptGate(); gate.start("old")
        repeat(4) { assertTrue(gate.admit("old", "$it", 15000, 0)) }
        gate.cancel()
        repeat(4) { assertFalse(gate.finish("old", "$it")) }
        assertFalse(gate.admit("old", "late", 15000, 0))
        gate.start("new")
        repeat(4) { assertTrue(gate.admit("new", "$it", 15000, 0)) }
        repeat(4) { assertFalse(gate.finish("old", "$it")) }
        assertFalse(gate.admit("new", "overflow", 15000, 0))
        repeat(4) { assertTrue(gate.finish("new", "$it")) }
    }
    @Test fun rejectedAdmissionDoesNotReserveCapacityAndDeadlineIsBounded() {
        val gate = AttemptGate(); gate.start("one")
        assertFalse(gate.admit("one", " ", 15000, 0))
        assertFalse(gate.admit("one", "expired", 1000, 1000))
        assertFalse(gate.admit("one", "too-long", 61001, 1000))
        assertTrue(gate.admit("one", "max", 61000, 1000))
        assertFalse(gate.admit("one", "max", 61000, 1000))
        repeat(3) { assertTrue(gate.admit("one", "$it", 15000, 0)) }
        assertFalse(gate.admit("one", "fifth", 15000, 0))
    }
    @Test(expected = IllegalArgumentException::class) fun digestIsNotTranscript() {
        SigningPolicy.validate("vpn", ByteArray(32))
    }
    @Test(expected = IllegalArgumentException::class) fun rejectPaddedBase64() {
        SigningPolicy.decode("AA==")
    }
    private fun hex(value: String) = value.chunked(2).map { it.toInt(16).toByte() }.toByteArray()
}

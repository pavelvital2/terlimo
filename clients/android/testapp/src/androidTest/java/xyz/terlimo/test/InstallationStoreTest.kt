package xyz.terlimo.test

import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith
import java.security.KeyFactory
import java.security.Signature
import java.security.spec.X509EncodedKeySpec

/** Requires a separately authorized physical/emulator run; never run implicitly by assembly. */
@RunWith(AndroidJUnit4::class)
class InstallationStoreTest {
    @Test fun installationKeySignsTranscriptAndRemainsStable() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val store = InstallationStore(context)
        val key = store.publicSpki()
        assertArrayEquals(key, InstallationStore(context).publicSpki())
        val transcript = "WLBS-POP-1\u0000".toByteArray() + ByteArray(96) { it.toByte() }
        val proof = store.sign("bootstrap", transcript)
        assertTrue(Signature.getInstance("SHA256withECDSA").run {
            initVerify(KeyFactory.getInstance("EC").generatePublic(X509EncodedKeySpec(key)))
            update(transcript); verify(proof)
        })
    }
    @Test fun encryptedStateRoundTripsWithoutReplacingIdentity() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val store = InstallationStore(context)
        val original = store.read()
        val id = store.installationId()
        try {
            store.write(JSONObject().put("test_marker", "synthetic-only"))
            assertEquals("synthetic-only", store.read().getString("test_marker"))
            assertEquals(id, store.installationId())
        } finally { store.write(original) }
    }
}

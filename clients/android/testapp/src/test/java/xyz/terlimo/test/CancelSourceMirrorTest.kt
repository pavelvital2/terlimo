package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class CancelSourceMirrorTest {
    @Test
    fun `sanitized cancel-source marker is mirrored with the fixed vocabulary`() {
        for (token in NativeStderrCodes.VPNSOURCE_TOKENS) {
            val m = NativeStderrCodes.match("vpnstage: CANCEL_SOURCE $token")
            assertEquals("vpnstage:CANCEL_SOURCE:$token", m?.value)
            assertEquals(false, m?.terminal)
        }
    }

    @Test
    fun `unknown or removed token and malformed lines are rejected`() {
        assertNull(NativeStderrCodes.match("vpnstage: CANCEL_SOURCE SOMETHING_ELSE"))
        assertNull(NativeStderrCodes.match("vpnstage: CANCEL_SOURCE WORKER_ERROR"))
        assertNull(NativeStderrCodes.match("vpnstage: CANCEL_SOURCE"))
        assertNull(NativeStderrCodes.match("vpnstage: OTHER HOST_STOP"))
    }

    @Test
    fun `runtime-cancel origin and usage stage are mirrored with fixed vocabularies`() {
        for (token in NativeStderrCodes.VPNSOURCE_TOKENS) {
            val m = NativeStderrCodes.match("vpnstage: RUNTIME_CANCEL $token")
            assertEquals("vpnstage:RUNTIME_CANCEL:$token", m?.value)
            assertEquals(false, m?.terminal)
        }
        for (stage in NativeStderrCodes.USAGESTAGE_TOKENS) {
            assertEquals("usagestage:$stage", NativeStderrCodes.match("usagestage: $stage")?.value)
        }
        assertNull(NativeStderrCodes.match("usagestage: RESPONSE_1XX"))
        assertNull(NativeStderrCodes.match("usagestage: SOMETHING_ELSE"))
        assertNull(NativeStderrCodes.match("vpnstage: RUNTIME_CANCEL raw secret"))
    }
}

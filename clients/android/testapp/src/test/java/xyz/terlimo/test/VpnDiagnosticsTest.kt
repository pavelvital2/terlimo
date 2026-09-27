package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Test

class VpnDiagnosticsTest {
    @Test fun acceptsOnlyClosedStageAndErrorPairs() {
        assertEquals(
            mapOf("vpn_stage" to "HANDSHAKE", "vpn_error_class" to "TIMEOUT", "vpn_auth_code" to "NONE"),
            VpnDiagnostics.parse("HANDSHAKE", "TIMEOUT", "NONE")
        )
        for ((stage, error) in listOf(
            "private endpoint" to "TIMEOUT",
            "HANDSHAKE" to "private native failure",
            1 to "FAILED",
            "CONFIG" to 1
        )) assertNull(VpnDiagnostics.parse(stage, error, "NONE"))
        assertNull(VpnDiagnostics.parse("AUTH", "FAILED", "private raw error"))
        assertEquals("LEASE_CONFLICT", VpnDiagnostics.parse("AUTH", "FAILED", "LEASE_CONFLICT")?.get("vpn_auth_code"))
    }

    @Test fun userReceiptIncludesClosedVpnEvidenceWithoutRawInput() {
        val text = UserDiagnostics.describe(emptyMap(), emptyMap(), emptyMap(),
            mapOf("vpn_stage" to "CONFIG", "vpn_error_class" to "CANCELED", "raw" to "private"))
        assertEquals(true, text.contains("vpn_stage: CONFIG"))
        assertEquals(true, text.contains("vpn_error_class: CANCELED"))
        assertFalse(text.contains("private"))
    }
}

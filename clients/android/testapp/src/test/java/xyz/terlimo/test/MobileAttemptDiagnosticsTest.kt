package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class MobileAttemptDiagnosticsTest {
    @Test fun normalizesSchemeHostPortAndStripsUserinfoPathQuery() {
        assertEquals("https://terlimo.193-5-251-217.sslip.io:443",
            MobileBaseUrl.normalize("https://terlimo.193-5-251-217.sslip.io"))
        assertEquals("https://terlimo.example.test:8443",
            MobileBaseUrl.normalize("https://user:token@terlimo.example.test:8443/api/mobile/v1?key=abc#frag"))
        assertEquals("http://10.0.0.7:8080", MobileBaseUrl.normalize("http://10.0.0.7:8080/path"))
    }

    @Test fun rejectsNonHttpOutOfBoundsAndMalformedValues() {
        assertNull(MobileBaseUrl.normalize(""))
        assertNull(MobileBaseUrl.normalize("not a url"))
        assertNull(MobileBaseUrl.normalize("ftp://host/path"))
        assertNull(MobileBaseUrl.normalize("https://host:0"))
        assertNull(MobileBaseUrl.normalize("https://host:70000"))
        assertNull(MobileBaseUrl.normalize("https://" + "a".repeat(129)))
        assertNull(MobileBaseUrl.normalize("https://ho st"))
    }

    @Test fun lineCarriesOnlyHex64AndNormalizedEndpoint() {
        val id = "a".repeat(64)
        val line = MobileAttemptDiagnostics.line(id,
            "https://user:sekrit@terlimo.example.test:8443/api/mobile/v1?token=sekrit")
        assertEquals("mobile_attempt installation_id=$id endpoint=https://terlimo.example.test:8443", line)
        assertFalse(line.contains("sekrit"))
        assertFalse(line.contains("/api"))
        assertFalse(line.contains("token"))
        assertFalse(line.contains('\n'))
        assertTrue(line.length <= MobileAttemptDiagnostics.MAX_LINE)
    }

    @Test fun nonHex64AndInvalidEndpointDegradeToFixedPlaceholders() {
        assertEquals("mobile_attempt installation_id=unavailable endpoint=invalid",
            MobileAttemptDiagnostics.line("NOT-HEX", ";;;"))
        assertEquals("mobile_attempt installation_id=${"0".repeat(64)} endpoint=https://host:443",
            MobileAttemptDiagnostics.line("0".repeat(64), "HTTPS://HOST"))
    }
}

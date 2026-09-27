package xyz.terlimo.test

import java.io.IOException
import java.net.SocketTimeoutException
import java.net.UnknownHostException
import javax.net.ssl.SSLException
import org.junit.Assert.*
import org.junit.Test

class ReadinessEvidenceTest {
    private fun complete() = ReadinessEvidence().apply {
        stage = ReadinessEvidence.Stage.READY
        vpnPresent = true
        dnsOk = true
        httpsOk = true
        httpStatusOk = true
        expectedExitOk = true
        statsOk = true
        handshakePresent = true
        handshakeFresh = true
        rx = 1L
        tx = 1L
    }

    @Test fun eachIncompleteStageHasItsOwnFailureCode() {
        val evidence = ReadinessEvidence()
        assertEquals("VPN_APPLY_FAILED", evidence.failureCode())
        evidence.stage = ReadinessEvidence.Stage.VPN_NETWORK
        assertEquals("VPN_NETWORK_UNAVAILABLE", evidence.failureCode())
        evidence.vpnPresent = true
        evidence.stage = ReadinessEvidence.Stage.DNS
        assertEquals("DNS_FAILED", evidence.failureCode())
        evidence.dnsOk = true
        evidence.stage = ReadinessEvidence.Stage.HTTPS
        assertEquals("HTTPS_FAILED", evidence.failureCode())
        evidence.httpsOk = true
        evidence.stage = ReadinessEvidence.Stage.HTTP_STATUS
        assertEquals("HTTP_STATUS_FAILED", evidence.failureCode())
        evidence.httpStatusOk = true
        evidence.stage = ReadinessEvidence.Stage.EXPECTED_EXIT
        assertEquals("EXPECTED_EXIT_MISMATCH", evidence.failureCode())
        evidence.expectedExitOk = true
        evidence.stage = ReadinessEvidence.Stage.WG_STATS
        assertEquals("WG_STATS_FAILED", evidence.failureCode())
        evidence.statsOk = true
        assertEquals("WG_NO_HANDSHAKE", evidence.failureCode())
        evidence.handshakePresent = true
        assertEquals("WG_STALE_HANDSHAKE", evidence.failureCode())
        evidence.handshakeFresh = true
        assertEquals("WG_NO_TRAFFIC", evidence.failureCode())
        assertFalse(evidence.ready)
        evidence.rx = 1L
        evidence.tx = 1L
        evidence.stage = ReadinessEvidence.Stage.READY
        assertEquals("READY", evidence.failureCode())
        assertTrue(evidence.ready)
    }

    @Test fun everyRequiredFactIndependentlyPreventsReady() {
        val cases: List<Pair<String, ReadinessEvidence.() -> Unit>> = listOf(
            "VPN_NETWORK_UNAVAILABLE" to { vpnPresent = false },
            "DNS_FAILED" to { dnsOk = false },
            "HTTPS_FAILED" to { httpsOk = false },
            "HTTP_STATUS_FAILED" to { httpStatusOk = false },
            "EXPECTED_EXIT_MISMATCH" to { expectedExitOk = false },
            "WG_STATS_FAILED" to { statsOk = false },
            "WG_NO_HANDSHAKE" to { handshakePresent = false },
            "WG_STALE_HANDSHAKE" to { handshakeFresh = false },
            "WG_NO_TRAFFIC" to { rx = 0L },
            "WG_NO_TRAFFIC" to { tx = 0L },
            "WG_NO_TRAFFIC" to { rx = -1L },
            "WG_NO_TRAFFIC" to { tx = -1L },
        )
        for ((expectedCode, change) in cases) {
            val evidence = complete().apply(change)
            assertFalse(expectedCode, evidence.ready)
            assertEquals(expectedCode, evidence.failureCode())
        }
    }

    @Test fun wgSamplingCannotHideAnEarlierFailedProbe() {
        val evidence = complete().apply {
            stage = ReadinessEvidence.Stage.WG_STATS
            dnsOk = false
            httpsOk = false
            statsOk = false
        }
        assertEquals("DNS_FAILED", evidence.failureCode())
        evidence.vpnPresent = false
        assertEquals("VPN_NETWORK_UNAVAILABLE", evidence.failureCode())
    }

    @Test fun stageIsDiagnosticAndCannotSubstituteForFacts() {
        for (stage in ReadinessEvidence.Stage.values()) {
            assertTrue(complete().apply { this.stage = stage }.ready)
        }
        assertFalse(ReadinessEvidence().apply { stage = ReadinessEvidence.Stage.READY }.ready)
    }

    @Test fun deadlineDoesNotWeakenReadinessOrReplaceSpecificFailure() {
        val evidence = complete().apply { handshakeFresh = false }
        val before = evidence.snapshot()
        val expired = evidence.snapshot(deadline = true)
        assertEquals(false, before["deadline"])
        assertEquals(true, expired["deadline"])
        assertEquals(false, expired["ready"])
        assertEquals("WG_STALE_HANDSHAKE", expired["failureCode"])
        assertEquals(before.filterKeys { it != "deadline" }, expired.filterKeys { it != "deadline" })
        assertEquals(true, complete().snapshot(deadline = true)["ready"])
    }

    @Test fun snapshotIsDetachedAndKeepsLongCounters() {
        val evidence = complete().apply { rx = Long.MAX_VALUE; tx = Long.MAX_VALUE }
        val snapshot = evidence.snapshot()
        evidence.rx = 0L
        evidence.stage = ReadinessEvidence.Stage.DNS
        assertEquals(Long.MAX_VALUE, snapshot["rx"])
        assertEquals(Long.MAX_VALUE, snapshot["tx"])
        assertEquals("READY", snapshot["stage"])
        assertEquals(true, snapshot["ready"])
        assertFalse(evidence.ready)
    }

    @Test fun exceptionSnapshotsContainOnlySafeCategoriesNeverRawSecrets() {
        val syntheticSecret = "synthetic-token-private-key-response-body"
        val cases = listOf(
            SocketTimeoutException(syntheticSecret) to "TIMEOUT",
            UnknownHostException(syntheticSecret) to "DNS",
            SSLException(syntheticSecret) to "TLS",
            IOException(syntheticSecret) to "IO",
            SecurityException(syntheticSecret) to "SECURITY",
            IllegalStateException(syntheticSecret) to "OTHER",
            object : RuntimeException(syntheticSecret) {
                override fun toString() = syntheticSecret
            } to "OTHER",
        )
        assertEquals("NONE", ReadinessEvidence().snapshot()["exceptionClass"])
        for ((error, expectedClass) in cases) {
            val evidence = ReadinessEvidence()
            evidence.recordException(error)
            val snapshot = evidence.snapshot()
            assertEquals(expectedClass, snapshot["exceptionClass"])
            assertFalse(snapshot.toString().contains(syntheticSecret))
            assertFalse(snapshot.toString().contains(error.javaClass.name))
            assertTrue(snapshot.values.all { it is String || it is Boolean || it is Long })
        }
    }
}

package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test
import java.net.InetAddress

class TestVpnPolicyTest {
    @Test fun connectedRequiresFreshHandshakeAndBothTrafficDirections() {
        assertTrue(TestVpnPolicy.ready(1, 1, 1000, 1000, 1001))
        assertFalse(TestVpnPolicy.ready(0, 1, 1000, 1000, 1001))
        assertFalse(TestVpnPolicy.ready(1, 0, 1000, 1000, 1001))
        assertFalse(TestVpnPolicy.ready(1, 1, 999, 1000, 1001))
        assertFalse(TestVpnPolicy.ready(1, 1, 1002, 1000, 1001))
    }
    @Test fun ipv4ProfileAccepted() {
        val ipv4 = listOf(InetAddress.getByAddress(byteArrayOf(10, 0, 0, 1)))
        TestVpnPolicy.requireIpv4(ipv4, ipv4, ipv4)
    }
    @Test fun ipv6InAnyFieldRejected() {
        val ipv4 = listOf(InetAddress.getByAddress(byteArrayOf(10, 0, 0, 1)))
        val ipv6 = listOf(InetAddress.getByAddress(ByteArray(16)))
        for (fields in listOf(listOf(ipv6, ipv4, ipv4), listOf(ipv4, ipv6, ipv4), listOf(ipv4, ipv4, ipv6))) {
            try { TestVpnPolicy.requireIpv4(fields[0], fields[1], fields[2]); fail("IPv6 accepted") }
            catch (_: IllegalArgumentException) {}
        }
    }
    @Test fun lateStateAfterCancellationCannotPublish() {
        val gate = AttemptGate(); gate.start("old")
        var state = "Connecting"
        assertTrue(gate.ifActive("old") { state = "ConfiguringVPN" })
        gate.cancel(); state = "Stopping"
        assertFalse(gate.ifActive("old") { state = "Connected" })
        assertEquals("Stopping", state)
        gate.start("new")
        assertFalse(gate.ifActive("old") { state = "Connected" })
    }
    @Test fun workerMetadataIsTypedAndCanonical() {
        SigningPolicy.validateWorker("bootstrap", "bootstrap")
        SigningPolicy.validateWorker("vpn", "0")
        SigningPolicy.validateWorker("vpn", "35")
        for ((kind, worker) in listOf("bootstrap" to "0", "vpn" to "worker-0", "vpn" to "01", "vpn" to "native")) {
            try { SigningPolicy.validateWorker(kind, worker); fail("Invalid worker accepted") }
            catch (_: IllegalArgumentException) {}
        }
    }
}

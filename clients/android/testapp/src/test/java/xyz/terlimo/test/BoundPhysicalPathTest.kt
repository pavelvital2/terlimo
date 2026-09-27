package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class BoundPhysicalPathTest {
    @Test fun defaultOrValidationChangeDoesNotCancelBoundTransport() {
        assertFalse(PhysicalNetworkRecovery.boundPathUnavailable(NetworkCandidate(true, true, false, false)))
        assertFalse(PhysicalNetworkRecovery.boundPathUnavailable(NetworkCandidate(true, true, true, false)))
    }
    @Test fun actualCapabilityLossRequiresRecovery() {
        assertTrue(PhysicalNetworkRecovery.boundPathUnavailable(NetworkCandidate(false, true, true, true)))
        assertTrue(PhysicalNetworkRecovery.boundPathUnavailable(NetworkCandidate(true, false, true, true)))
    }
}

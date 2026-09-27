package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class PhysicalAvailabilityTest {
    @Test fun authoritativeLossRejectsStaleSnapshotAndLateCapabilities() {
        val availability = PhysicalAvailability<String>()
        val cached = NetworkCandidate(true, true, true, true)
        availability.lost("lost")
        val snapshots = listOf("lost") // Android may still return this in allNetworks.
        assertNull(snapshots.firstOrNull { availability.accepts(it) && cached.usable })
        assertFalse(availability.accepts("lost"))
        availability.available("new")
        var state = PhysicalNetworkRecovery.begin("node", 0, 0)
        var schedules = 0
        for (candidate in listOf("lost", "new", "new")) {
            if (availability.accepts(candidate)) {
                PhysicalNetworkRecovery.ready(state, state.generation, cached, 1)?.let {
                    state = PhysicalNetworkRecovery.admit(it.state)
                    schedules++
                }
            }
        }
        assertEquals(1, schedules)
        assertFalse(availability.accepts("lost"))
    }
    @Test fun onlyLaterAvailableReadmitsHandle() {
        val availability = PhysicalAvailability<String>()
        availability.lost("old")
        assertFalse(availability.accepts("old"))
        availability.available("old")
        assertTrue(availability.accepts("old"))
    }
}

package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * The admission reason must keep the exact `canStart` contract while naming the rejected
 * boundary for the bounded switch diagnostics.
 */
class SwitchAdmissionTest {
    private val connected = ViewState(
        phase = "Connected",
        nodes = listOf(NodeLabel("A", "Node A"), NodeLabel("B", "Node B")),
        selectedNodeId = "A",
        catalogRevision = "7",
    )

    @Test fun admissionNamesEveryRejectedBoundary() {
        assertEquals(SwitchAdmission.ALLOWED, ActiveNodeSwitch.admission(connected, "B"))
        assertEquals(SwitchAdmission.NOT_CONNECTED,
            ActiveNodeSwitch.admission(connected.copy(phase = "CatalogReady"), "B"))
        assertEquals(SwitchAdmission.PENDING,
            ActiveNodeSwitch.admission(connected.copy(pendingNodeId = "B"), "B"))
        assertEquals(SwitchAdmission.PENDING,
            ActiveNodeSwitch.admission(connected.copy(pendingSwitchId = "switch"), "B"))
        assertEquals(SwitchAdmission.NO_REVISION,
            ActiveNodeSwitch.admission(connected.copy(catalogRevision = ""), "B"))
        assertEquals(SwitchAdmission.SAME_TARGET, ActiveNodeSwitch.admission(connected, "A"))
        assertEquals(SwitchAdmission.UNKNOWN_TARGET, ActiveNodeSwitch.admission(connected, "missing"))
    }

    @Test fun canStartIsExactlyTheAllowedAdmission() {
        val variants = listOf(
            connected,
            connected.copy(phase = "CatalogReady"),
            connected.copy(pendingNodeId = "B"),
            connected.copy(pendingSwitchId = "switch"),
            connected.copy(catalogRevision = ""),
            connected.copy(selectedNodeId = "B"),
            connected.copy(nodes = emptyList()),
        )
        for (state in variants) {
            for (target in listOf("A", "B", "missing")) {
                assertEquals(
                    "state=${state.phase} target=$target",
                    ActiveNodeSwitch.admission(state, target) == SwitchAdmission.ALLOWED,
                    ActiveNodeSwitch.canStart(state, target),
                )
            }
        }
    }
}

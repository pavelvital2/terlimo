package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class MobileCatalogGateTest {
    private val noRight = listOf("none", "restricted_checkout")
    private val dataRight = listOf("onboarding_hour", "subscription_data")

    @Test fun beginWaitsForCatalogWithOneArmedWindow() {
        val gate = MobileCatalogGate()
        assertEquals(MobileCatalogAction.ARM, gate.onAttemptStart("a", mobile = true))
        assertEquals(MobileCatalogState.WAITING_CATALOG, gate.stateOf("a"))
    }

    @Test fun noRightDisarmsOnceAndStaysWaitingRight() {
        val gate = MobileCatalogGate()
        gate.onAttemptStart("a", mobile = true)
        assertEquals(MobileCatalogAction.DISARM, gate.onAccountAccess("none", "a"))
        assertEquals(MobileCatalogState.WAITING_RIGHT, gate.stateOf("a"))
        for (access in noRight) {
            assertEquals(MobileCatalogAction.NONE, gate.onAccountAccess(access, "a"))
            assertEquals(MobileCatalogState.WAITING_RIGHT, gate.stateOf("a"))
        }
    }

    @Test fun dataRightArmsOnlyOnTheTransitionOutOfWaitingRight() {
        val gate = MobileCatalogGate()
        gate.onAttemptStart("a", mobile = true)
        for (access in dataRight) {
            assertEquals(MobileCatalogAction.NONE, gate.onAccountAccess(access, "a"))
            assertEquals(MobileCatalogState.WAITING_CATALOG, gate.stateOf("a"))
        }
        for (access in dataRight) {
            assertEquals(MobileCatalogAction.DISARM, gate.onAccountAccess("none", "a"))
            assertEquals(MobileCatalogAction.ARM, gate.onAccountAccess(access, "a"))
            assertEquals(MobileCatalogState.WAITING_CATALOG, gate.stateOf("a"))
            assertEquals(MobileCatalogAction.NONE, gate.onAccountAccess(access, "a"))
        }
    }

    @Test fun catalogAcceptanceIsTerminalAndNeverReArms() {
        val gate = MobileCatalogGate()
        gate.onAttemptStart("a", mobile = true)
        gate.onAccountAccess("none", "a")
        gate.onAccountAccess("subscription_data", "a")
        assertEquals(MobileCatalogAction.DISARM, gate.onCatalogAccepted("a"))
        assertEquals(MobileCatalogState.CATALOG_ACCEPTED, gate.stateOf("a"))
        assertEquals(MobileCatalogAction.NONE, gate.onCatalogAccepted("a"))
        for (access in noRight + dataRight) {
            assertEquals(MobileCatalogAction.NONE, gate.onAccountAccess(access, "a"))
            assertEquals(MobileCatalogState.CATALOG_ACCEPTED, gate.stateOf("a"))
        }
    }

    @Test fun catalogAcceptanceWithoutAnyDataRightIsTerminalToo() {
        val gate = MobileCatalogGate()
        gate.onAttemptStart("a", mobile = true)
        assertEquals(MobileCatalogAction.DISARM, gate.onCatalogAccepted("a"))
        assertEquals(MobileCatalogState.CATALOG_ACCEPTED, gate.stateOf("a"))
        for (access in noRight + dataRight) {
            assertEquals(MobileCatalogAction.NONE, gate.onAccountAccess(access, "a"))
        }
    }

    @Test fun legacyAttemptsKeepTheTimerLifecycleAndIgnoreRights() {
        val gate = MobileCatalogGate()
        assertEquals(MobileCatalogAction.ARM, gate.onAttemptStart("legacy", mobile = false))
        assertEquals(MobileCatalogState.WAITING_CATALOG, gate.stateOf("legacy"))
        for (access in noRight + dataRight) {
            assertEquals(MobileCatalogAction.NONE, gate.onAccountAccess(access, "legacy"))
            assertEquals(MobileCatalogState.WAITING_CATALOG, gate.stateOf("legacy"))
        }
        assertEquals(MobileCatalogAction.DISARM, gate.onCatalogAccepted("legacy"))
    }

    @Test fun staleEventsForAnOldAttemptAreFenced() {
        val gate = MobileCatalogGate()
        gate.onAttemptStart("a", mobile = true)
        gate.onAttemptStart("b", mobile = true)
        for (access in noRight + dataRight) {
            assertEquals(MobileCatalogAction.NONE, gate.onAccountAccess(access, "a"))
        }
        assertEquals(MobileCatalogAction.NONE, gate.onCatalogAccepted("a"))
        assertEquals(MobileCatalogAction.NONE, gate.onAttemptStop("a"))
        assertEquals(MobileCatalogState.WAITING_CATALOG, gate.stateOf("b"))
        assertEquals(MobileCatalogState.IDLE, gate.stateOf("a"))
        assertEquals(MobileCatalogAction.DISARM, gate.onAttemptStop("b"))
        assertEquals(MobileCatalogState.IDLE, gate.stateOf("b"))
    }

    @Test fun unknownRightsChangeNothing() {
        val gate = MobileCatalogGate()
        gate.onAttemptStart("a", mobile = true)
        assertEquals(MobileCatalogAction.NONE, gate.onAccountAccess("", "a"))
        assertEquals(MobileCatalogAction.NONE, gate.onAccountAccess("future_value", "a"))
        assertEquals(MobileCatalogAction.NONE, gate.onAccountAccess("NONE", "a"))
        assertEquals(MobileCatalogState.WAITING_CATALOG, gate.stateOf("a"))
        gate.onAccountAccess("none", "a")
        assertEquals(MobileCatalogAction.NONE, gate.onAccountAccess("future_value", "a"))
        assertEquals(MobileCatalogState.WAITING_RIGHT, gate.stateOf("a"))
    }

    @Test fun boundedActionSetHasNoTerminalConnectOrStopAction() {
        assertEquals(setOf("ARM", "DISARM", "NONE"), MobileCatalogAction.values().map { it.name }.toSet())
        assertFalse(MobileCatalogAction.values().any {
            it.name in setOf("STOP", "OFF", "TERMINAL", "CLEAR", "REVOKE", "CONNECT")
        })
    }

    @Test fun restrictedCheckoutKeepsControlMeaningAndCanStillBecomeData() {
        // restricted_checkout only lifts the catalogue expectation: it is not terminal and the
        // projection keeps its checkout/control meaning; a later data right still transitions.
        val gate = MobileCatalogGate()
        gate.onAttemptStart("a", mobile = true)
        assertEquals(MobileCatalogAction.DISARM, gate.onAccountAccess("restricted_checkout", "a"))
        assertEquals(MobileCatalogState.WAITING_RIGHT, gate.stateOf("a"))
        assertEquals(MobileCatalogAction.ARM, gate.onAccountAccess("onboarding_hour", "a"))
        assertEquals(MobileCatalogState.WAITING_CATALOG, gate.stateOf("a"))
    }
}

package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class RegistrationLoginTest {
    @Test fun loginVisibilityKeepsHourAndPaidBranchesUntouched() {
        assertTrue(RegistrationUi.loginVisibleFor("none", "none", false))
        assertTrue(RegistrationUi.loginVisibleFor("pending", null, false))
        assertFalse(RegistrationUi.loginVisibleFor("none", "onboarding_hour", false))
        assertFalse(RegistrationUi.loginVisibleFor("registered", "subscription", false))
        assertFalse(RegistrationUi.loginVisibleFor("none", "none", true))
    }
}

class RegistrationLoginGateTest {
    @Test fun coldTapStartsOneAttemptAndSendsExactlyOnceAfterVerifiedRights() {
        val gate = RegistrationLoginGate()
        assertEquals(RegistrationLoginTapAction.START_SERVICE, gate.onTap(null, true))
        assertEquals(RegistrationLoginTapAction.IGNORE, gate.onTap(null, true))
        assertTrue(gate.onAttemptStarted("a1"))
        assertTrue(gate.onVerifiedRights("a1"))
        assertFalse(gate.onVerifiedRights("a1"))
        assertEquals(RegistrationLoginTapAction.IGNORE, gate.onTap(null, true))
        assertTrue(gate.onResolved("a1"))
        assertEquals(RegistrationLoginTapAction.START_SERVICE, gate.onTap(null, true))
    }

    @Test fun liveAttemptSendsOnceAndResolutionDoesNotStopTheHostAttempt() {
        val gate = RegistrationLoginGate()
        assertEquals(RegistrationLoginTapAction.SEND_NOW, gate.onTap("live", true))
        assertEquals(RegistrationLoginTapAction.IGNORE, gate.onTap("live", true))
        assertFalse(gate.onResolved("live"))
        assertEquals(RegistrationLoginTapAction.SEND_NOW, gate.onTap("live", true))
    }

    @Test fun foreignAttemptAndIneligibleTapChangeNothing() {
        val gate = RegistrationLoginGate()
        assertEquals(RegistrationLoginTapAction.ERROR, gate.onTap(null, false))
        assertEquals(RegistrationLoginTapAction.START_SERVICE, gate.onTap(null, true))
        assertTrue(gate.onAttemptStarted("a2"))
        assertFalse(gate.onVerifiedRights("other"))
        assertFalse(gate.onTimeout("other"))
        assertFalse(gate.onResolved("other"))
        assertTrue(gate.onTimeout("a2"))
        assertEquals(RegistrationLoginTapAction.START_SERVICE, gate.onTap(null, true))
    }
}

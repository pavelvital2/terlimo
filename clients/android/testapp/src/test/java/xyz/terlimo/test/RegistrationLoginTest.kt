package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class RegistrationLoginTest {
    @Test fun loginVisibilityKeepsHourAndPaidBranchesUntouched() {
        assertTrue(RegistrationUi.loginVisibleFor(false, "none", false))
        assertTrue(RegistrationUi.loginVisibleFor(false, null, false))
        assertFalse(RegistrationUi.loginVisibleFor(false, "onboarding_hour", false))
        assertFalse(RegistrationUi.loginVisibleFor(true, "subscription", false))
        assertFalse(RegistrationUi.loginVisibleFor(false, "none", true))
    }
}

class RegistrationLoginGateTest {
    @Test fun coldActionsAreSingleFlightAndKeepTheirWireOperation() {
        for (action in RegistrationAction.entries) {
            val gate = RegistrationLoginGate()
            assertEquals(RegistrationLoginTapAction.START_SERVICE, gate.onTap(null, action, true))
            assertEquals(RegistrationLoginTapAction.IGNORE, gate.onTap(null, RegistrationAction.LOGIN, true))
            assertTrue(gate.onAttemptStarted("a1"))
            assertEquals(RegistrationVerified.Send(action), gate.onVerifiedRights("a1") { true })
            assertEquals(RegistrationVerified.Ignore, gate.onVerifiedRights("a1") { true })
            assertTrue(gate.expects("a1", action == RegistrationAction.REFRESH))
            assertFalse(gate.expects("a1", action != RegistrationAction.REFRESH))
            assertTrue(gate.onResolved("a1"))
            assertFalse(gate.onResolved("a1"))
        }
        assertEquals("refresh_telegram_registration", RegistrationAction.REFRESH.wireType)
        assertEquals("request_telegram_registration", RegistrationAction.REGISTER.wireType)
    }

    @Test fun freshEligibilityCanRefuseRegisterOrLoginButRefreshNeverBecomesALinkRequest() {
        for (action in listOf(RegistrationAction.REGISTER, RegistrationAction.LOGIN)) {
            val gate = RegistrationLoginGate()
            assertEquals(RegistrationLoginTapAction.START_SERVICE, gate.onTap(null, action, true))
            gate.onAttemptStarted("cold")
            assertEquals(RegistrationVerified.Refused, gate.onVerifiedRights("cold") { false })
            assertTrue(gate.onResolved("cold"))
        }
        val refresh = RegistrationLoginGate()
        refresh.onTap(null, RegistrationAction.REFRESH, true)
        refresh.onAttemptStarted("r")
        assertEquals(RegistrationVerified.Send(RegistrationAction.REFRESH), refresh.onVerifiedRights("r") { false })
        assertFalse(refresh.expects("r", false))
        assertTrue(refresh.onResolved("r"))
    }

    @Test fun liveAttemptResolutionAndTimeoutNeverClaimTheHostVpn() {
        val gate = RegistrationLoginGate()
        assertEquals(RegistrationLoginTapAction.SEND_NOW, gate.onTap("live", RegistrationAction.REGISTER, true))
        val first = gate.token()
        assertFalse(gate.isCold("live"))
        assertFalse(gate.onResolved("live"))
        assertEquals(RegistrationLoginTapAction.SEND_NOW, gate.onTap("live", RegistrationAction.REFRESH, true))
        assertFalse(gate.onTimeout("live", first)) // old timer cannot cancel the successor
        assertTrue(gate.beginRecovery("live"))
        assertFalse(gate.finishRecovery("live"))
        assertFalse(gate.isCold("live"))
    }

    @Test fun staleAttemptCannotSendResolveOrReleaseTheSuccessor() {
        val gate = RegistrationLoginGate()
        assertEquals(RegistrationLoginTapAction.ERROR, gate.onTap(null, RegistrationAction.REGISTER, false))
        gate.onTap(null, RegistrationAction.REGISTER, true)
        gate.onAttemptStarted("old")
        assertTrue(gate.onTimeout("old"))
        gate.onTap(null, RegistrationAction.REFRESH, true)
        gate.onAttemptStarted("new")
        assertEquals(RegistrationVerified.Ignore, gate.onVerifiedRights("old") { true })
        assertFalse(gate.onTimeout("old"))
        assertFalse(gate.onResolved("old"))
        assertEquals(RegistrationVerified.Send(RegistrationAction.REFRESH), gate.onVerifiedRights("new") { true })
        assertTrue(gate.beginRecovery("new"))
        assertFalse(gate.expects("new", true)) // duplicate status cannot re-send recovery
        assertFalse(gate.finishRecovery("old"))
        assertTrue(gate.finishRecovery("new"))
        assertFalse(gate.onTimeout("new"))
    }

    @Test fun startFailureAndRecoveryTimeoutReleaseOnlyTheirOwnOperation() {
        val gate = RegistrationLoginGate()
        gate.onTap(null, RegistrationAction.REGISTER, true)
        assertTrue(gate.onTimeout(null))
        gate.onTap(null, RegistrationAction.REFRESH, true)
        gate.onAttemptStarted("cold")
        gate.onVerifiedRights("cold") { true }
        gate.beginRecovery("cold")
        assertTrue(gate.isCold("cold"))
        assertFalse(gate.onTimeout("foreign"))
        assertTrue(gate.onTimeout("cold"))
        assertFalse(gate.finishRecovery("cold"))
    }
}

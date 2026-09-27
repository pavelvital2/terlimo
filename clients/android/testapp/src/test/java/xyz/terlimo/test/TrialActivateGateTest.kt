package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class TrialActivateGateTest {
    @Test fun `cold tap starts one service-only attempt and sends once after fresh me`() {
        val gate = TrialActivateGate()
        assertEquals(TrialTapAction.START_SERVICE, gate.onTap(null, canActivate = true))
        // duplicate cold tap is coalesced
        assertEquals(TrialTapAction.IGNORE, gate.onTap(null, canActivate = true))
        assertTrue(gate.onAttemptStarted("a1"))
        // no send before a confirmed /me
        assertFalse(gate.onVerifiedRights("a1", registrationConfirmed = false, canActivate = true))
        assertFalse(gate.onVerifiedRights("a1", registrationConfirmed = true, canActivate = false))
        assertTrue(gate.onVerifiedRights("a1", registrationConfirmed = true, canActivate = true))
        // exactly once
        assertFalse(gate.onVerifiedRights("a1", registrationConfirmed = true, canActivate = true))
        // cold attempt is released on terminal; a pre-existing attempt would not be
        assertTrue(gate.onTerminal("a1"))
        assertEquals(TrialTapAction.START_SERVICE, gate.onTap(null, canActivate = true))
    }

    @Test fun `active attempt sends immediately and is not stopped on terminal`() {
        val gate = TrialActivateGate()
        assertEquals(TrialTapAction.SEND_NOW, gate.onTap("live", canActivate = true))
        assertEquals(TrialTapAction.IGNORE, gate.onTap("live", canActivate = true))
        assertFalse("active attempts are never stopped by the trial terminal", gate.onTerminal("live"))
        // after terminal the fence resets and a new tap can act again
        assertEquals(TrialTapAction.SEND_NOW, gate.onTap("live", canActivate = true))
    }

    @Test fun `active attempt without can_activate is a fixed error, not a silent no-op`() {
        val gate = TrialActivateGate()
        assertEquals(TrialTapAction.ERROR, gate.onTap("live", canActivate = false))
    }

    @Test fun `control failure clears the pending start and surfaces an error`() {
        val gate = TrialActivateGate()
        assertEquals(TrialTapAction.START_SERVICE, gate.onTap(null, canActivate = true))
        assertTrue(gate.onControlFailed())
        assertFalse(gate.onControlFailed())
        assertEquals(TrialTapAction.START_SERVICE, gate.onTap(null, canActivate = true))
    }

    @Test fun `timeout releases the cold attempt exactly once`() {
        val gate = TrialActivateGate()
        assertEquals(TrialTapAction.START_SERVICE, gate.onTap(null, canActivate = true))
        assertTrue(gate.onAttemptStarted("a1"))
        assertTrue(gate.onTimeout("a1"))
        assertFalse(gate.onTimeout("a1"))
    }

    @Test fun `cancellation on stop or new attempt fences older callbacks`() {
        val gate = TrialActivateGate()
        assertEquals(TrialTapAction.START_SERVICE, gate.onTap(null, canActivate = true))
        assertTrue(gate.onAttemptStarted("a1"))
        gate.reset()
        assertFalse(gate.onVerifiedRights("a1", registrationConfirmed = true, canActivate = true))
        assertFalse(gate.onTimeout("a1"))
    }
}

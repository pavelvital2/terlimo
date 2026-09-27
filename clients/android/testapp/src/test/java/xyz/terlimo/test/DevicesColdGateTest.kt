package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class DevicesColdGateTest {
    private val list = DevicesRequest.List("devlist-1")
    private val del = DevicesRequest.Delete("devdel-1", "dev-1", "terlimo-device-key-0001")

    @Test fun idleTapStartsOneColdAttemptAndSendsExactlyOnceAfterVerifiedRights() {
        val gate = DevicesColdGate()
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, list))
        assertEquals(DevicesTapAction.IGNORE, gate.onTap(null, true, list))
        assertTrue(gate.onAttemptStarted("a1"))
        val verified = gate.onVerifiedRights("a1", true)
        assertTrue(verified is DevicesVerified.Send)
        assertEquals(list, (verified as DevicesVerified.Send).request)
        assertEquals(DevicesVerified.NotCold, gate.onVerifiedRights("a1", true))
        assertTrue(gate.onResult("a1"))
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, list))
    }

    @Test fun foreignAttemptCannotConsumeTheQueuedRequest() {
        val gate = DevicesColdGate()
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, list))
        assertTrue(gate.onAttemptStarted("a1"))
        assertEquals(DevicesVerified.NotCold, gate.onVerifiedRights("other", true))
        val verified = gate.onVerifiedRights("a1", true)
        assertTrue(verified is DevicesVerified.Send)
        assertEquals(list, (verified as DevicesVerified.Send).request)
    }

    @Test fun nonManageableTapIsRefusedAndColdAttemptWithoutRightsTimesOut() {
        val gate = DevicesColdGate()
        assertEquals(DevicesTapAction.ERROR, gate.onTap("a1", false, list))
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, list))
        assertTrue(gate.onAttemptStarted("a2"))
        assertFalse(gate.onTimeout("other"))
        assertTrue(gate.onTimeout("a2"))
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, list))
    }

    @Test fun refusedVerifiedRightsReleaseTheOwnedColdAttempt() {
        val gate = DevicesColdGate()
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, list))
        assertTrue(gate.onAttemptStarted("a3"))
        assertEquals(DevicesVerified.Refused, gate.onVerifiedRights("a3", false))
        assertTrue(gate.isCold("a3"))
        assertFalse(gate.onRefused("foreign"))
        assertTrue(gate.onRefused("a3"))
        assertFalse(gate.isCold("a3"))
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, list))
    }

    @Test fun abandonedColdStartReleasesItsQueuedRequest() {
        val gate = DevicesColdGate()
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, list))
        assertEquals(DevicesTapAction.IGNORE, gate.onTap(null, true, del))
        assertTrue(gate.onTimeout(null))
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, list))
        assertTrue(gate.onAttemptStarted("a5"))
        assertTrue(gate.onVerifiedRights("a5", true) is DevicesVerified.Send)
    }

    @Test fun activeAttemptSendsImmediatelyAndIsSingleFlightUntilResult() {
        val gate = DevicesColdGate()
        assertEquals(DevicesTapAction.SEND_NOW, gate.onTap("live", true, del))
        assertEquals(DevicesTapAction.IGNORE, gate.onTap("live", true, list))
        assertFalse(gate.onResult("other"))
        assertFalse(gate.onResult("live"))
        assertEquals(DevicesTapAction.SEND_NOW, gate.onTap("live", true, list))
    }

    @Test fun deleteRequestKeepsItsHostOwnedKeyAndCorrelation() {
        val gate = DevicesColdGate()
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, del))
        assertTrue(gate.onAttemptStarted("a4"))
        val verified = gate.onVerifiedRights("a4", true)
        assertTrue(verified is DevicesVerified.Send)
        val request = (verified as DevicesVerified.Send).request as DevicesRequest.Delete
        assertEquals("devdel-1", request.requestId)
        assertEquals("dev-1", request.deviceId)
        assertEquals("terlimo-device-key-0001", request.idempotencyKey)
        assertTrue(gate.onResult("a4"))
    }
}

class DevicesColdSequenceTest {
    private val del = DevicesRequest.Delete("devdel-9", "dev-9", "terlimo-device-key-0009")

    @Test fun coldDeleteIsSentOnceWithItsOwnIdentity() {
        val gate = DevicesColdGate()
        val ui = DevicesPolicy.reset()
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, del))
        assertTrue(gate.onAttemptStarted("cold-1"))
        val verified = gate.onVerifiedRights("cold-1", true)
        assertTrue(verified is DevicesVerified.Send)
        val request = (verified as DevicesVerified.Send).request as DevicesRequest.Delete
        assertEquals(del, request)
        // The service records exactly this own request at send time; it stays sendable.
        val pending = DevicesPolicy.beginDelete(ui, request.requestId, request.deviceId, "cold-1", "acct")
        assertTrue(DevicesPolicy.canSendDelete(pending, request.requestId))
        assertFalse(DevicesPolicy.canSendDelete(pending, "devdel-foreign"))
        // No duplicate send: the gate refuses a second tap while the result is outstanding.
        assertEquals(DevicesTapAction.IGNORE, gate.onTap(null, true, del))
        assertTrue(gate.onResult("cold-1"))
    }

    @Test fun lateCallbackFromRetiredAttemptCannotStopOrClearTheNewOne() {
        val gate = DevicesColdGate()
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, del))
        assertTrue(gate.onAttemptStarted("old"))
        // A late verified-rights callback from a retired attempt is not cold and changes nothing.
        assertEquals(DevicesVerified.NotCold, gate.onVerifiedRights("retired", true))
        assertEquals(DevicesTapAction.IGNORE, gate.onTap(null, true, del))
        assertFalse(gate.onTimeout("retired"))
        assertTrue(gate.isCold("old"))
        // The owned attempt times out: owned stop only, the foreign late callback still cannot.
        assertTrue(gate.onTimeout("old"))
        assertFalse(gate.onResult("retired"))
        assertEquals(DevicesTapAction.START_SERVICE, gate.onTap(null, true, del))
    }
}

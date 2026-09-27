package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class DevicesLifecycleSourceTest {
    private fun source(path: String): String = listOf(File(path), File("testapp/$path"), File("../$path"))
        .first { it.isFile }.readText()

    @Test fun coldStartDoesNotPreArmTheInFlightRequestAndKeepsStrictSendGuards() {
        val service = source("testapp/src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(service.contains("val liveAttempt = gate.active?.takeIf { native != null }"))
        assertEquals(2, service.split("DevicesTapAction.START_SERVICE -> {\n                        startDevicesColdAttempt()").size - 1)
        assertFalse(service.contains("DevicesPolicy.beginDelete(\n                            view.devices, request.requestId, deviceId, liveAttempt.orEmpty()"))
        assertFalse(service.contains("DevicesPolicy.beginList(\n                            view.devices, requestId, liveAttempt.orEmpty()"))
        assertTrue(service.contains("DevicesPolicy.canSendList(view.devices, requestId)"))
        assertTrue(service.contains("DevicesPolicy.canSendDelete(devices, request.requestId)"))
    }

    @Test fun refusalTimeoutAndReplacedAttemptReleaseOnlyTheOwnedColdState() {
        val service = source("testapp/src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(service.contains("if (devicesGate.onRefused(attempt)) stopAttempt(null)"))
        assertTrue(service.contains("gate.active == attempt && !stopping.get() &&"))
        assertTrue(service.contains("devicesGate.isCold(attempt) && devicesGate.onTimeout(attempt)"))
        assertTrue(service.contains("else devicesGate.onResult(liveAttempt)"))
        assertTrue(service.contains("devicesGate.onTimeout(null)"))
        val gate = source("testapp/src/main/java/xyz/terlimo/test/DevicesColdGate.kt")
        assertTrue(gate.contains("fun onRefused(attemptId: String): Boolean"))
        assertTrue(gate.contains("fun isCold(attemptId: String?): Boolean"))
        assertTrue(gate.contains("if (!canManage) return DevicesVerified.Refused"))
    }
}

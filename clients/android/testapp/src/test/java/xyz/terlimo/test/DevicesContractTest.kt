package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

class DevicesContractTest {
    private fun list(json: String) = JSONObject(json)

    private fun validList() = """
    {"v":1,"attempt_id":"a","type":"devices_list_result","state":"ok","client_request_id":"devlist-1",
     "request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-27T10:00:00Z",
     "schema_version":"1.0","device_limit":2,"slots_used":1,"revision":"7",
     "devices":[{"device_id":"dev-1","name":"Pixel","platform":"android","is_current":true,"status":"active","bound_at":"2026-09-27T09:00:00Z","revoked_at":null},
                {"device_id":"dev-2","platform":"android","is_current":false,"status":"revoked"}]}
    """.trimIndent()

    private fun validDelete() = """
    {"v":1,"attempt_id":"a","type":"device_delete_result","state":"ok","client_request_id":"devdel-1",
     "request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-27T10:00:00Z",
     "schema_version":"1.0","status":"pending","operation_id":"op-1","slot_released":true,
     "access_application_state":"pending","residual_access_lease_seconds":600}
    """.trimIndent()

    @Test fun listParsesCanonicalShape() {
        val parsed = DevicesContract.parse(list(validList())) as DevicesEvent.List
        assertEquals("devlist-1", parsed.clientRequestId)
        assertEquals(2, parsed.list.devices.size)
        assertNull(parsed.list.devices[1].name)
        assertNull(parsed.list.devices[1].boundAt)
        assertEquals("android", parsed.list.devices[1].platform)
    }

    @Test fun listAcceptsCanonicalVariantsAndRejectsMalformed() {
        listOf(
            validList().replace("\"status\":\"active\"", "\"status\":\"pending\""),
            validList().replace("\"status\":\"revoked\"", "\"status\":\"deactivated\""),
            validList().replace("\"device_limit\":2", "\"device_limit\":0"),
            validList().replace("\"device_limit\":2,\"slots_used\":1", "\"device_limit\":1,\"slots_used\":3"),
        ).forEach { DevicesContract.parse(list(it)) }
        listOf(
            validList().replace("\"platform\":\"android\"", "\"platform\":\"ios\""),
            validList().replace("\"name\":\"Pixel\"", "\"name\":\"" + "x".repeat(65) + "\""),
            validList().replace("\"status\":\"active\"", "\"status\":\"weird\""),
            validList().replace("\"device_id\":\"dev-2\"", "\"device_id\":\"dev-1\""),
            validList().replace("\"is_current\":true,", ""),
            validList().replace("\"client_request_id\":\"devlist-1\",", ""),
        ).forEach { assertThrows(Exception::class.java) { DevicesContract.parse(list(it)) } }
    }

    @Test fun deleteParsesStrictlyAndRejectsMalformed() {
        val parsed = DevicesContract.parse(list(validDelete())) as DevicesEvent.Delete
        assertEquals("pending", parsed.result.status)
        assertEquals("devdel-1", parsed.result.clientRequestId)
        assertTrue(parsed.result.slotReleased)
        assertEquals(600, parsed.result.residualAccessLeaseSeconds)
        listOf(
            validDelete().replace("\"status\":\"pending\"", "\"status\":\"done\""),
            validDelete().replace("\"access_application_state\":\"pending\"", "\"access_application_state\":\"made_up\""),
            validDelete().replace("\"residual_access_lease_seconds\":600", "\"residual_access_lease_seconds\":-1"),
            validDelete().replace("\"client_request_id\":\"devdel-1\",", ""),
        ).forEach { assertThrows(Exception::class.java) { DevicesContract.parse(list(it)) } }
    }

    @Test fun errorsCarryTheHostCorrelationId() {
        val listError = DevicesContract.parse(list(
            """{"v":1,"attempt_id":"a","type":"devices_list_result","state":"error","code":"TRANSPORT","client_request_id":"devlist-9"}"""))
        assertEquals(DevicesEvent.Failure("devices_list_result", "TRANSPORT", "devlist-9"), listError)
        val deleteError = DevicesContract.parse(list(
            """{"v":1,"attempt_id":"a","type":"device_delete_result","state":"error","code":"DEVICE_REMOVED","client_request_id":"devdel-9"}"""))
        assertEquals(DevicesEvent.Failure("device_delete_result", "DEVICE_REMOVED", "devdel-9"), deleteError)
    }
}

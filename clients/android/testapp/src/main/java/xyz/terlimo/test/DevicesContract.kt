package xyz.terlimo.test

import org.json.JSONObject

/** One connected device (§§18–19), a display-only projection of the server list. */
internal data class DeviceRow(
    val deviceId: String,
    val name: String?,
    val platform: String,
    val isCurrent: Boolean,
    val status: String,
    val boundAt: String?,
    val revokedAt: String?,
)

/** Strict GET /devices projection. */
internal data class DevicesList(
    val devices: List<DeviceRow>,
    val deviceLimit: Int,
    val slotsUsed: Int,
    val revision: String,
)

/** Strict DELETE /devices/{id} projection; `status=="pending"` is not an applied revoke. */
internal data class DeviceDeleteResult(
    val clientRequestId: String,
    val status: String,
    val operationId: String,
    val slotReleased: Boolean,
    val accessApplicationState: String,
    val residualAccessLeaseSeconds: Int?,
)

internal sealed class DevicesEvent {
    data class List(val clientRequestId: String?, val list: DevicesList) : DevicesEvent()
    data class Delete(val result: DeviceDeleteResult) : DevicesEvent()
    /** A bounded error; carries the host-owned correlation id of the failed list/delete. */
    data class Failure(
        val event: String,
        val code: String,
        val clientRequestId: String? = null,
    ) : DevicesEvent()
}

/**
 * Strict host decode of the §§18–19 devices bridge events, matched to the canonical
 * devices.json. The server owns device state; the host only renders accepted results.
 */
internal object DevicesContract {
    const val EVENT_LIST = "devices_list_result"
    const val EVENT_DELETE = "device_delete_result"

    private val LIST_KEYS = setOf(
        "v", "attempt_id", "type", "state", "client_request_id", "request_id", "server_time",
        "schema_version", "device_limit", "slots_used", "revision", "devices",
    )
    private val DELETE_KEYS = setOf(
        "v", "attempt_id", "type", "state", "client_request_id", "request_id", "server_time",
        "schema_version", "status", "operation_id", "slot_released", "access_application_state",
        "residual_access_lease_seconds",
    )
    private val LIST_ERROR_KEYS = setOf("v", "attempt_id", "type", "state", "code", "client_request_id")
    private val DELETE_ERROR_KEYS = setOf("v", "attempt_id", "type", "state", "code", "client_request_id")
    // Canonical Device: required device_id/platform/is_current/status; name/bound_at/revoked_at
    // are optional strings|null (name <= 64). optional fields may be absent.
    private val DEVICE_REQUIRED = setOf("device_id", "platform", "is_current", "status")
    private val DEVICE_OPTIONAL = setOf("name", "bound_at", "revoked_at")
    private val STATUSES = setOf("active", "pending", "revoked", "deactivated")
    private val APPLICATION_STATES =
        setOf("not_requested", "pending", "applied", "retryable_failure", "rejected")
    private val ID = Regex("^[A-Za-z0-9._~-]{1,128}$")

    fun parse(event: JSONObject): DevicesEvent {
        val type = event.getString("type")
        if (event.optString("state") == "error") {
            return when (type) {
                EVENT_LIST -> {
                    check(event.keys().asSequence().toSet() == LIST_ERROR_KEYS) { "DEVICES_INVALID" }
                    DevicesEvent.Failure(type, event.getString("code"), event.getString("client_request_id"))
                }
                EVENT_DELETE -> {
                    check(event.keys().asSequence().toSet() == DELETE_ERROR_KEYS) { "DEVICES_INVALID" }
                    DevicesEvent.Failure(type, event.getString("code"), event.getString("client_request_id"))
                }
                else -> error("DEVICES_INVALID")
            }
        }
        return when (type) {
            EVENT_LIST -> DevicesEvent.List(event.getString("client_request_id"), parseList(event))
            EVENT_DELETE -> DevicesEvent.Delete(parseDelete(event))
            else -> error("DEVICES_INVALID")
        }
    }

    private fun optionalText(entry: JSONObject, key: String, maxLength: Int): String? {
        if (!entry.has(key) || entry.isNull(key)) return null
        val value = entry.opt(key)
        check(value is String && value.length <= maxLength) { "DEVICES_INVALID" }
        return value
    }

    private fun parseList(event: JSONObject): DevicesList {
        check(event.keys().asSequence().toSet() == LIST_KEYS &&
            event.getString("schema_version") == "1.0") { "DEVICES_INVALID" }
        val limit = event.getInt("device_limit")
        val used = event.getInt("slots_used")
        // Canonical: both are 0..100 and independent (no slots <= limit constraint).
        check(limit in 0..100 && used in 0..100) { "DEVICES_INVALID" }
        val revision = event.getString("revision")
        check(revision.matches(Regex("^(0|[1-9][0-9]{0,18})$"))) { "DEVICES_INVALID" }
        val array = event.getJSONArray("devices")
        val seen = HashSet<String>()
        val devices = buildList(array.length()) {
            repeat(array.length()) { index ->
                val entry = array.getJSONObject(index)
                val keys = entry.keys().asSequence().toSet()
                check(keys.containsAll(DEVICE_REQUIRED) && DEVICE_OPTIONAL.containsAll(keys - DEVICE_REQUIRED) &&
                    keys - DEVICE_REQUIRED - DEVICE_OPTIONAL == emptySet<String>()) { "DEVICES_INVALID" }
                val id = entry.getString("device_id")
                check(ID.matches(id) && seen.add(id)) { "DEVICES_INVALID" }
                check(entry.getString("platform") == "android") { "DEVICES_INVALID" }
                val status = entry.getString("status")
                check(status in STATUSES) { "DEVICES_INVALID" }
                add(DeviceRow(
                    id,
                    optionalText(entry, "name", 64),
                    "android",
                    entry.getBoolean("is_current"),
                    status,
                    optionalText(entry, "bound_at", 64),
                    optionalText(entry, "revoked_at", 64),
                ))
            }
        }
        return DevicesList(devices, limit, used, revision)
    }

    private fun parseDelete(event: JSONObject): DeviceDeleteResult {
        check(event.keys().asSequence().toSet() == DELETE_KEYS &&
            event.getString("schema_version") == "1.0") { "DEVICES_INVALID" }
        val status = event.getString("status")
        check(status == "ok" || status == "pending") { "DEVICES_INVALID" }
        val applicationState = event.getString("access_application_state")
        check(applicationState in APPLICATION_STATES) { "DEVICES_INVALID" }
        val operationId = event.getString("operation_id")
        check(operationId.isNotEmpty() && operationId.length <= 128) { "DEVICES_INVALID" }
        val requestId = event.getString("client_request_id")
        check(requestId.isNotEmpty() && requestId.length <= 128) { "DEVICES_INVALID" }
        val residual = event.opt("residual_access_lease_seconds")?.takeIf { !JSONObject.NULL.equals(it) }?.let {
            check(it is Int && it >= 0) { "DEVICES_INVALID" }; it
        }
        return DeviceDeleteResult(requestId, status, operationId, event.getBoolean("slot_released"),
            applicationState, residual)
    }
}

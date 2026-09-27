package xyz.terlimo.test

import org.json.JSONArray
import org.json.JSONObject
import java.time.Instant

/**
 * S5 §11 host-side strict contract of the native announcements events
 * (`announcements_list`, `announcement_read`). Native already validated the wire schema;
 * this parser is a second closed boundary so a malformed/hostile frame can never become
 * UI state. It never invents a server date, delivery status, count or action.
 */
internal data class AnnouncementAction(val type: String, val label: String?) {
    companion object {
        const val NONE = "none"
        const val REFRESH_CATALOG = "refresh_catalog"
        const val OPEN_PAYMENTS = "open_payments"
        const val OPEN_SUPPORT = "open_support"
        val TYPES = setOf(NONE, REFRESH_CATALOG, OPEN_PAYMENTS, OPEN_SUPPORT)
    }
}

internal data class Announcement(
    val id: String,
    val title: String,
    val text: String,
    val action: AnnouncementAction,
    val validUntil: String?,
    val unread: Boolean,
    val revision: String,
)

internal data class AnnouncementsSnapshot(
    val requestId: String,
    val serverTime: String,
    val announcements: List<Announcement>,
    val unreadCount: Int,
)

internal data class AnnouncementReadAck(
    val announcementId: String,
    val read: Boolean,
    val readAt: String,
)

internal object AnnouncementsCodec {
    /** Any string 1..128 is a legal contract id; only unreserved ids are forwardable. */
    private val PATH_ID = Regex("^[A-Za-z0-9._~-]{1,128}$")
    private val REQUEST_ID = Regex("^[0-9a-f]{32}$")
    private val UTC_TIME = Regex("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$")

    fun unreservedPathId(id: String): Boolean =
        id.isNotEmpty() && id != "." && id != ".." && !id.contains("..") && PATH_ID.matches(id)

    private fun keys(event: JSONObject): Set<String> = event.keys().asSequence().toSet()

    /** A non-null string with the exact JSON type and a bounded length (empty allowed). */
    private fun stringField(event: JSONObject, key: String, max: Int, min: Int = 0): String {
        check(event.has(key) && !event.isNull(key)) { "ANNOUNCEMENTS_INVALID" }
        val value = event.get(key)
        // JSONObject.getString coerces numbers/bools, so require the real JSON string type.
        check(value is String && value.length in min..max) { "ANNOUNCEMENTS_INVALID" }
        return value
    }

    /** A non-null JSON integer (coercion of "1"/1.5 is rejected). */
    private fun intField(event: JSONObject, key: String): Int {
        check(event.has(key) && !event.isNull(key)) { "ANNOUNCEMENTS_INVALID" }
        val value = event.get(key)
        check(value is Int) { "ANNOUNCEMENTS_INVALID" }
        return value
    }

    /** A non-null JSON boolean (coercion of "true" is rejected). */
    private fun boolField(event: JSONObject, key: String): Boolean {
        check(event.has(key) && !event.isNull(key)) { "ANNOUNCEMENTS_INVALID" }
        val value = event.get(key)
        check(value is Boolean) { "ANNOUNCEMENTS_INVALID" }
        return value
    }

    private fun validUtc(value: String): Boolean = UTC_TIME.matches(value)

    private fun validRequestId(value: String): Boolean = REQUEST_ID.matches(value)

    /** Strict parse of one `announcements_list` ok frame. */
    fun parseList(event: JSONObject): AnnouncementsSnapshot {
        val allowed = setOf(
            "v", "attempt_id", "type", "state", "request_id", "server_time", "schema_version",
            "announcements", "unread_count",
        )
        check(keys(event).all { it in allowed } && keys(event).containsAll(
            setOf("request_id", "server_time", "schema_version", "announcements", "unread_count"))) {
            "ANNOUNCEMENTS_INVALID"
        }
        check(event.optString("state") == "ok") { "ANNOUNCEMENTS_INVALID" }
        check(event.optString("schema_version") == "1.0") { "ANNOUNCEMENTS_INVALID" }
        val requestId = stringField(event, "request_id", 32, min = 1)
        check(validRequestId(requestId)) { "ANNOUNCEMENTS_INVALID" }
        val serverTime = stringField(event, "server_time", 64, min = 1)
        check(validUtc(serverTime)) { "ANNOUNCEMENTS_INVALID" }
        val unreadCount = intField(event, "unread_count")
        check(unreadCount >= 0) { "ANNOUNCEMENTS_INVALID" }
        check(event.has("announcements") && !event.isNull("announcements")) { "ANNOUNCEMENTS_INVALID" }
        val array: JSONArray = event.getJSONArray("announcements")
        val list = ArrayList<Announcement>(array.length())
        for (index in 0 until array.length()) {
            list += parseAnnouncement(array.getJSONObject(index))
        }
        return AnnouncementsSnapshot(requestId, serverTime, list, unreadCount)
    }

    private fun parseAnnouncement(json: JSONObject): Announcement {
        val allowed = setOf("announcement_id", "title", "text", "action", "valid_until", "unread", "revision")
        check(json.keys().asSequence().toSet().all { it in allowed }) { "ANNOUNCEMENTS_INVALID" }
        val id = stringField(json, "announcement_id", 128, min = 1)
        val title = stringField(json, "title", 128)
        val text = stringField(json, "text", 4096)
        val revision = stringField(json, "revision", 19, min = 1)
        check(AccountAccessValues.validRevision(revision)) { "ANNOUNCEMENTS_INVALID" }
        val unread = boolField(json, "unread")
        check(json.has("action") && !json.isNull("action")) { "ANNOUNCEMENTS_INVALID" }
        val action = parseAction(json.getJSONObject("action"))
        val validUntil: String? = if (!json.has("valid_until") || json.isNull("valid_until")) {
            null
        } else {
            stringField(json, "valid_until", 64)
        }
        return Announcement(id, title, text, action, validUntil, unread, revision)
    }

    private fun parseAction(json: JSONObject): AnnouncementAction {
        check(json.keys().asSequence().toSet().all { it in setOf("type", "label") }) { "ANNOUNCEMENTS_INVALID" }
        val type = stringField(json, "type", 32, min = 1)
        check(type in AnnouncementAction.TYPES) { "ANNOUNCEMENTS_INVALID" }
        val label: String? = if (!json.has("label") || json.isNull("label")) {
            null
        } else {
            stringField(json, "label", 64)
        }
        return AnnouncementAction(type, label)
    }

    /** Strict parse of one `announcement_read` ok frame, distinct from an error frame. */
    fun parseRead(event: JSONObject): AnnouncementReadAck {
        val allowed = setOf(
            "v", "attempt_id", "type", "state", "request_id", "server_time", "schema_version",
            "announcement_id", "read", "read_at",
        )
        check(keys(event).all { it in allowed }) { "ANNOUNCEMENTS_INVALID" }
        check(event.optString("state") == "ok") { "ANNOUNCEMENTS_INVALID" }
        check(event.optString("schema_version") == "1.0") { "ANNOUNCEMENTS_INVALID" }
        check(validRequestId(stringField(event, "request_id", 32, min = 1))) { "ANNOUNCEMENTS_INVALID" }
        check(validUtc(stringField(event, "server_time", 64, min = 1))) { "ANNOUNCEMENTS_INVALID" }
        val id = stringField(event, "announcement_id", 128, min = 1)
        val read = boolField(event, "read")
        val readAt = stringField(event, "read_at", 64, min = 1)
        check(validUtc(readAt)) { "ANNOUNCEMENTS_INVALID" }
        return AnnouncementReadAck(id, read, readAt)
    }

    /**
     * True when a nullable `valid_until` is in the past and the announcement must be
     * hidden. A null or unparseable value is never treated as expired: the host does not
     * invent a date, and hiding a live message is worse than showing it.
     */
    fun isExpired(validUntil: String?, nowEpochMs: Long): Boolean {
        if (validUntil == null) return false
        val instant = runCatching { Instant.parse(validUntil) }.getOrNull() ?: return false
        return instant.toEpochMilli() < nowEpochMs
    }

    fun visible(list: List<Announcement>, nowEpochMs: Long): List<Announcement> =
        list.filter { !isExpired(it.validUntil, nowEpochMs) }
}

/** Minimal revision check shared with the contract (decimal, no leading zero). */
internal object AccountAccessValues {
    private val REVISION = Regex("^(0|[1-9][0-9]{0,18})$")
    fun validRevision(value: String): Boolean = REVISION.matches(value)
}

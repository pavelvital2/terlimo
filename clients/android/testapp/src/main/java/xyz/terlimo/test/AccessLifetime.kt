package xyz.terlimo.test

import org.json.JSONObject
import java.time.Instant

/** Exact projection of authenticated v1 finite or v2 explicit-null access. */
internal data class AccessLifetime(val expiresAt: Instant?, val remainingMillis: Long?)

internal object AccessLifetimeParser {
    fun parse(event: JSONObject, now: Instant = Instant.now()): AccessLifetime {
        if (event.opt("access_unlimited") == true) {
            require(event.has("access_expires_at") && event.isNull("access_expires_at")) { "LEASE_SCHEMA_INVALID" }
            return AccessLifetime(null, null)
        }
        val end = Instant.parse(event.getString("access_expires_at"))
        val server = Instant.parse(event.getString("server_time"))
        val remaining = minOf(end.toEpochMilli() - now.toEpochMilli(), end.toEpochMilli() - server.toEpochMilli())
        require(remaining in 1..900_000) { "LEASE_EXPIRED" }
        return AccessLifetime(end, remaining)
    }
}

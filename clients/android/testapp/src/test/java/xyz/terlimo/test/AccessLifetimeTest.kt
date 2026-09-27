package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import java.time.Instant

class AccessLifetimeTest {
    private val now = Instant.parse("2026-09-13T06:00:00Z")

    @Test fun finiteV1RemainsBounded() {
        val result = AccessLifetimeParser.parse(JSONObject()
            .put("access_expires_at", "2026-09-13T06:10:00Z")
            .put("server_time", "2026-09-13T06:00:00Z"), now)
        assertEquals(600_000L, result.remainingMillis)
        assertNotNull(result.expiresAt)
    }

    @Test fun explicitNullUnlimitedHasNoSyntheticDeadline() {
        val result = AccessLifetimeParser.parse(JSONObject()
            .put("access_expires_at", JSONObject.NULL)
            .put("access_unlimited", true)
            .put("server_time", "2026-09-13T06:00:00Z"), now)
        assertNull(result.remainingMillis)
        assertNull(result.expiresAt)
    }

    @Test fun missingFlagMissingFieldAndFakeValuesFailClosed() {
        val cases = listOf(
            JSONObject().put("access_expires_at", JSONObject.NULL).put("server_time", now.toString()),
            JSONObject().put("access_unlimited", true).put("server_time", now.toString()),
            JSONObject().put("access_unlimited", true).put("access_expires_at", "0").put("server_time", now.toString()),
            JSONObject().put("access_expires_at", "9999-12-31T23:59:59Z").put("server_time", now.toString()),
        )
        cases.forEach { assertTrue(runCatching { AccessLifetimeParser.parse(it, now) }.isFailure) }
    }
}

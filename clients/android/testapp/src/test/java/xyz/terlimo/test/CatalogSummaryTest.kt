package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import java.time.Instant
import java.time.ZoneOffset

class CatalogSummaryTest {
    private fun event() = JSONObject().put("subscription_status", "active").put("slots_used", 2).put("slots_limit", 2)
        .put("issued_at", "2026-09-11T05:00:00Z").put("catalog_expires_at", "2026-09-11T05:15:00Z")
        .put("subscription_expires_at", "2026-09-12T00:00:00Z")

    @Test fun separatesSubscriptionAndCatalogExpiryWithoutInventingDates() {
        val summary = CatalogSummary.parse(event())
        val text = summary.description(Instant.parse("2026-09-11T06:00:00Z"), ZoneOffset.UTC)
        assertTrue(text.contains("Подписка до 12.09.2026"))
        assertTrue(text.contains("Срок каталога истёк"))
        assertTrue(text.contains("Места: 2 из 2"))
        assertFalse(text.contains("Срок подписки истёк"))
        assertTrue(summary.description(Instant.parse("2026-09-12T00:00:00Z")).contains("Срок подписки истёк"))
    }

    @Test fun rejectsMissingMalformedCoercedAndConflictingMetadata() {
        val cases = listOf(event().apply { remove("subscription_expires_at") }, event().put("slots_used", "2"),
            event().put("slots_limit", 1), event().put("slots_used", 0), event().put("slots_limit", 2147483648L),
            event().put("subscription_status", "revoked"), event().put("issued_at", "not a date"),
            event().put("catalog_expires_at", "2026-10-01T00:00:00Z"))
        for (input in cases) assertTrue(runCatching { CatalogSummary.parse(input) }.isFailure)
    }

    @Test fun summaryNeverCopiesSecretExtraFields() {
        val text = CatalogSummary.parse(event().put("password", "do-not-show").put("registration_id", "private-id"))
            .description(Instant.parse("2026-09-11T05:05:00Z"))
        assertFalse(text.contains("do-not-show")); assertFalse(text.contains("private-id"))
    }

    @Test fun v2ExplicitNullSubscriptionExpiryIsUnlimitedWithoutFakeDate() {
        val summary = CatalogSummary.parse(event()
            .put("catalog_version", 2)
            .put("subscription_unlimited", true)
            .put("subscription_expires_at", JSONObject.NULL))
        assertNull(summary.subscriptionExpiresAt)
        assertTrue(summary.description(Instant.parse("2126-09-11T05:05:00Z")).contains("без ограничения срока"))
    }

    @Test fun missingNullOrVersionMismatchNeverMeansUnlimited() {
        val cases = listOf(
            event().put("catalog_version", 2).put("subscription_expires_at", JSONObject.NULL),
            event().put("catalog_version", 2).put("subscription_unlimited", true).apply { remove("subscription_expires_at") },
            event().put("subscription_unlimited", true).put("subscription_expires_at", JSONObject.NULL),
            event().put("catalog_version", 2).put("subscription_unlimited", true).put("subscription_expires_at", "0"),
        )
        cases.forEach { assertTrue(runCatching { CatalogSummary.parse(it) }.isFailure) }
    }
}

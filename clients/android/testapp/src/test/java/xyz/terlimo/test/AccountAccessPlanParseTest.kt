package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class AccountAccessPlanParseTest {
    private fun event(planFragment: String): String = """
    {"v":1,"attempt_id":"attempt","type":"account_access","access_version":1,
     "server_time":"2026-09-25T10:00:00Z","session_generation":"2",
     "previous_session_generation":"1","access_revision":"9",
     "account":{"state":"ACTIVE_PAID","telegram_linked":true,"binding_status":"active",
        "management_only":false,"account_ref":"acc-1"},
     "entitlement":{"type":"paid","status":"active","valid_from":null,
        "valid_until":"2026-10-25T10:00:00Z","effective_device_limit":2,"slots_used":1,
        "revision":"9","perpetual_commercial":false$planFragment},
     "onboarding":{"state":"active","started_by":"server_confirmed_first_connection",
        "started_at":"2026-09-25T09:30:00Z","not_after":"2026-09-25T10:30:00Z","duration_seconds":3600,
        "one_time":true,"extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
        "requires_hardware_id":false,"unit":"installation_fingerprint",
        "post_telegram_identity":"account_history_correlation",
        "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
     "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
        "data_access":"subscription_data","effective_deadline":"2026-10-25T10:00:00Z"},
     "registration":{"state":"registered","within_hour":true,"trial_available":false,
        "trial_reason":null,"purchase_available":true}}
    """

    private fun parse(planFragment: String) = AccountAccessParser.parse(JSONObject(event(planFragment)))

    @Test
    fun `plan present parses strictly and title may be null`() {
        val p = parse(""","plan":{"id":"terlimo-30d","title":"30 дней","duration_code":"days:30"}""")
        assertEquals("terlimo-30d", p.entitlement.plan?.id)
        assertEquals("30 дней", p.entitlement.plan?.title)
        assertEquals("days:30", p.entitlement.plan?.durationCode)
        val nullTitle = parse(""","plan":{"id":"x","title":null,"duration_code":"days:30"}""")
        assertEquals("x", nullTitle.entitlement.plan?.id)
        assertNull(nullTitle.entitlement.plan?.title)
    }

    @Test
    fun `absent or json null plan is unknown and old events still parse`() {
        assertNull(parse("").entitlement.plan)             // old server, key absent
        assertNull(parse(""","plan":null""").entitlement.plan)
    }

    @Test
    fun `invalid plan shapes are rejected`() {
        val bad = listOf(
            ""","plan":{"id":"x","title":"t","duration_code":"days:30","extra":1}""",
            ""","plan":{"id":"","title":"t","duration_code":"days:30"}""",
            ""","plan":{"id":"x","title":"t"}""",
            ""","plan":{"id":"x","duration_code":"days:30"}""",
            ""","plan":{"id":"x","title":"t","duration_code":""}""",
        )
        for (fragment in bad) {
            assertTrue("must reject $fragment", runCatching { parse(fragment) }.isFailure)
        }
    }
}

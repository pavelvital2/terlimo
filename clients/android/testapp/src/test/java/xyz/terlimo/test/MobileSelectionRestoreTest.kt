package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class MobileSelectionRestoreTest {
    private fun eventJson(
        generation: String = "1",
        previous: String? = null,
        revision: String = "7",
        serverTime: String = "2026-09-21T12:00:00Z",
        deadline: String? = "2026-09-21T12:10:00Z",
        dataAccess: String = "subscription_data",
        onboardingState: String = "active",
        perpetualCommercial: Boolean = false,
        validUntil: String? = "2027-01-01T00:00:00Z",
        extraTopLevel: String = "",
    ): String {
        val previousValue = previous?.let { "\"$it\"" } ?: "null"
        val deadlineValue = deadline?.let { "\"$it\"" } ?: "null"
        val validUntilValue = validUntil?.let { "\"$it\"" } ?: "null"
        val startedAt = if (onboardingState == "not_started") "null" else "\"2026-09-21T10:00:00Z\""
        val notAfter = if (onboardingState == "not_started") "null" else "\"2026-09-21T11:00:00Z\""
        return """
        {"v":1,"attempt_id":"attempt","type":"account_access","access_version":1,
         "server_time":"$serverTime","session_generation":"$generation",
         "previous_session_generation":$previousValue,"access_revision":"$revision",
         "account":{"state":"ACTIVE_PAID","telegram_linked":true,"binding_status":"active",
            "management_only":false,"account_ref":"acc-1"},
         "entitlement":{"type":"paid","status":"active","valid_from":"2026-08-01T00:00:00Z",
            "valid_until":$validUntilValue,"effective_device_limit":5,"slots_used":2,
            "revision":"5","perpetual_commercial":$perpetualCommercial},
         "onboarding":{"state":"$onboardingState","started_by":"server_confirmed_first_connection",
            "started_at":$startedAt,"not_after":$notAfter,"duration_seconds":3600,"one_time":true,
            "extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
            "requires_hardware_id":false,"unit":"installation_fingerprint",
            "post_telegram_identity":"account_history_correlation",
            "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
         "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
            "data_access":"$dataAccess","effective_deadline":$deadlineValue}$extraTopLevel}
        """
    }


    @Test fun acknowledgedPreferenceSurvivesRestartAndNativeClearIsFinal() {
        val source = MobileBootstrapSeed("https://unused.invalid", "test", null)
        val account = AccountAccessPolicy.apply(null, AccountAccessParser.parse(JSONObject(eventJson())), 0)!!
        val nodes = listOf(NodeLabel("A", "A"), NodeLabel("B", "B"))
        val state = ViewState(phase = "CatalogReady", nodes = nodes, selectedNodeId = "A", accountAccess = account)
        val preference = MobileSelectionPreference.fromCatalog(state, "installation", source)!!
        val unrelated = JSONObject().put("payment_journal", "unchanged").put("routing_settings", "unchanged")
        val saved = MobileSelectionPreference.merge(unrelated, preference)
        // The same encrypted-state JSON serialization/read/start path used by the host.
        val restarted = JSONObject(saved.toString())
        val start = MobileSelectionPreference.forStart(restarted, "installation", source)!!
        assertEquals("A", start.getString("node_id"))
        assertEquals("acc-1", start.getString("account_ref"))
        assertNull(MobileSelectionPreference.forStart(restarted, "other", source))
        assertNull(MobileSelectionPreference.forStart(restarted, "installation", source.copy(baseUrl = "https://other.invalid")))
        assertNull(MobileSelectionPreference.forStart(restarted, "installation", source.copy(environment = "production")))
        // Exact native clear wins even if A still occurs in the list / retained UI / browse.
        for (remaining in listOf(nodes, nodes.drop(1))) {
            val cleared = NodeSelection.applyCatalog(state, NodeCatalog(remaining, "", "next"), null,
                nativeSelectionAuthoritative = true)
            assertEquals("", cleared.selectedNodeId)
            val clearSaved = MobileSelectionPreference.merge(restarted,
                MobileSelectionPreference.fromCatalog(cleared, "installation", source))
            assertNull(MobileSelectionPreference.forStart(JSONObject(clearSaved.toString()), "installation", source))
            assertEquals("unchanged", clearSaved.getString("payment_journal"))
            assertEquals("unchanged", clearSaved.getString("routing_settings"))
        }
        val switching = state.copy(phase = "SwitchingServer", pendingNodeId = "B",
            pendingSwitchId = "switch", pendingSwitchRevision = "next")
        val pendingCatalog = NodeSelection.applyCatalog(switching, NodeCatalog(nodes, "B", "next"), null,
            nativeSelectionAuthoritative = true)
        assertEquals("A", MobileSelectionPreference.fromCatalog(pendingCatalog, "installation", source)!!.getString("node_id"))
        val completed = ActiveNodeSwitch.success(pendingCatalog, "B", "B", "switch", "next")
        val switchedSaved = MobileSelectionPreference.merge(saved,
            MobileSelectionPreference.fromCatalog(completed, "installation", source))
        assertEquals("B", MobileSelectionPreference.forStart(JSONObject(switchedSaved.toString()),
            "installation", source)!!.getString("node_id"))
        val rejected = ActiveNodeSwitch.failure(pendingCatalog, "A", "B", "switch", "next", "FAILED")
        assertEquals("A", MobileSelectionPreference.fromCatalog(rejected, "installation", source)!!.getString("node_id"))
        val browse = state.copy(displayMode = CatalogDisplayMode.BROWSE, browseSelectedId = "B")
        assertNull(MobileSelectionPreference.fromCatalog(browse, "installation", source))
        assertEquals("", NodeSelection.applyCatalog(browse, NodeCatalog(nodes, "", "next"), null,
            nativeSelectionAuthoritative = true).selectedNodeId)
        assertNull(MobileSelectionPreference.forStart(JSONObject().put("catalog_cache",
            CatalogCacheCodec.encode(RetainedCatalog(nodes, "A", "old"))), "installation", source))
        assertNull(MobileSelectionPreference.fromCatalog(state.copy(selectedNodeId = ""), "installation", source))
        assertNull(MobileSelectionPreference.fromCatalog(state.copy(accountAccess = account.copy(current = false)), "installation", source))
    }
}

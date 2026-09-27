package xyz.terlimo.test

import java.nio.file.Files
import java.nio.file.Paths
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class RoutingSettingsPersistenceTest {
    private fun source(path: String) =
        String(Files.readAllBytes(Paths.get(path)), Charsets.UTF_8)

    @Test fun nativePersistPreservesRoutingSettingsAndOtherUserFields() {
        val current = JSONObject()
            .put("link", "old-link")
            .put("state_b64", "old-state")
            .put("routing_settings", "policy-v1")
            .put("selected_node_id", "keep")
        val next = SubscriptionStateReplacement.withSubscriptionState(current, "new-link", "new-state")
        assertEquals("policy-v1", next.getString("routing_settings"))
        assertEquals("keep", next.getString("selected_node_id"))
        assertEquals("new-link", next.getString("link"))
        assertEquals("new-state", next.getString("state_b64"))
    }

    @Test fun nativePersistOnEmptyStateStillWritesSubscription() {
        val next = SubscriptionStateReplacement.withSubscriptionState(JSONObject(), "l", "s")
        assertEquals("l", next.getString("link"))
        assertFalse(next.has("routing_settings"))
    }

    @Test fun importLinkRefreshRetainsRoutingSettings() {
        val current = JSONObject().put("link", "a").put("routing_settings", "policy").put("catalog", "c")
        val next = SubscriptionStateReplacement.withLink(current, "b")
        assertEquals("policy", next.getString("routing_settings"))
        assertEquals("c", next.getString("catalog"))
        assertEquals("b", next.getString("link"))
    }

    @Test fun sessionServiceUsesOwnerControlledPersistenceNotFullOverwrite() {
        val src = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(src.contains("storage.writeSubscriptionState(activeLink"))
        assertTrue(src.contains("storage.writeActiveLink(activeLink)"))
        assertFalse(src.contains("JSONObject().put(\"link\", activeLink).put(\"state_b64\""))
    }

    @Test fun storeSerializesEveryInstanceOnOneProcessWideLock() {
        val src = source("src/main/java/xyz/terlimo/test/InstallationStore.kt")
        assertTrue(src.contains("private val LOCK = Any()"))
        assertTrue(src.contains("synchronized(LOCK)"))
        assertFalse(src.contains("@Synchronized"))
    }
}

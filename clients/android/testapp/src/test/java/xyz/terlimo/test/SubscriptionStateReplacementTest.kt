package xyz.terlimo.test

import java.nio.file.Files
import java.nio.file.Paths
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class SubscriptionStateReplacementTest {
    @Test fun removesAllSubscriptionCatalogAndPendingStateButKeepsRoutingSettings() {
        val current = JSONObject()
            .put("link", "protected")
            .put("state_b64", "protected")
            .put("catalog", "old")
            .put("selected_node_id", "old-a")
            .put("pending_node_id", "old-b")
            .put("routing_settings", "user-routing")

        val replaced = SubscriptionStateReplacement.withoutSubscription(current)

        assertEquals(setOf("routing_settings"), replaced.keys().asSequence().toSet())
        assertEquals("user-routing", replaced.getString("routing_settings"))
        assertFalse(replaced.has("link"))
        assertFalse(replaced.has("state_b64"))
    }

    @Test fun emptyUserSettingsProduceEmptyFreshSubscriptionState() {
        val replaced = SubscriptionStateReplacement.withoutSubscription(
            JSONObject().put("link", "protected").put("state_b64", "protected"))
        assertTrue(replaced.keys().asSequence().none())
    }

    @Test fun retainedStorageHelperHasNoUserFacingReplacementAction() {
        val store = String(Files.readAllBytes(Paths.get("src/main/java/xyz/terlimo/test/InstallationStore.kt")))
            .substringAfter("fun replaceSubscription()")
            .substringBefore("fun readRoutingSettings(")
        assertTrue(store.contains("write(SubscriptionStateReplacement.withoutSubscription(current))"))
        assertFalse(store.contains("delete("))
        assertFalse(store.contains("signingAlias"))
        assertFalse(store.contains("encryptionAlias"))

        val ui = String(Files.readAllBytes(Paths.get("src/main/java/xyz/terlimo/test/MainActivity.kt")))
        assertFalse(ui.contains("text = \"Заменить подписку\""))
        assertFalse(ui.contains("setPositiveButton(\"Удалить и заменить\")"))
        assertFalse(ui.contains("SessionService.replaceSubscriptionIfUnchanged(expectedState)"))
        assertFalse(ui.contains("InstallationStore(this@MainActivity).replaceSubscription()"))
    }

    @Test fun phaseChangeWhileDialogIsOpenRejectsMutationAtCommitTime() {
        val expected = SessionService.view
        assertTrue(SessionService.replaceSubscriptionIfUnchanged(expected) {}) // publication after dialog opened
        var mutated = false
        assertFalse(SessionService.replaceSubscriptionIfUnchanged(expected) { mutated = true })
        assertFalse(mutated)
    }
}

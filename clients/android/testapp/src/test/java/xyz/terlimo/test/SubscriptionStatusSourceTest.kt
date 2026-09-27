package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class SubscriptionStatusSourceTest {
    private fun source(path: String): String = listOf(File(path), File("testapp/$path"))
        .first { it.isFile }.readText()

    @Test fun subscriptionScreenViewIsVisibleAndPopulated() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        val creation = activity.substringAfter("subscription = TextView(this)")
            .substringBefore("subscriptionPanel.addView(subscription)")
        assertTrue(creation.contains("visibility = View.VISIBLE"))
        assertFalse(creation.contains("View.GONE"))
        assertFalse(activity.contains("subscription.visibility = View.GONE"))

        val render = activity.substringAfter("private fun render(")
        assertTrue(render.contains("subscription.text = SubscriptionStatusText.status("))
        assertTrue(render.contains("state.summary"))
        assertTrue(render.contains("state.accountAccess"))
        assertTrue(render.contains("android.os.SystemClock.elapsedRealtime()"))

        // The visible status lives on the existing "Подписка" destination surface, not a new screen.
        val surface = activity.substringAfter("val subscriptionSurface = ScrollView(this)")
            .substringBefore("val helpSurface")
        assertTrue(surface.contains("addView(subscriptionPanel)"))
        assertTrue(activity.contains("subscriptionPanel.addView(subscription)"))
        assertTrue(activity.contains("destination(\"Подписка\""))
        assertTrue(activity.contains("subscriptionSurface.visibility = View.VISIBLE"))
    }

    @Test fun subscriptionStatusReintroducesNoTechnicalTransportLine() {
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        val statusPolicy = source("src/main/java/xyz/terlimo/test/SubscriptionStatusText.kt")
        val creation = activity.substringAfter("subscription = TextView(this)")
            .substringBefore("subscriptionPanel.addView(subscription)")
        val assignment = activity.substringAfter("subscription.text = SubscriptionStatusText.status(")
            .substringBefore("val idle =")
        listOf(creation, assignment, statusPolicy).forEach { snippet ->
            assertFalse(snippet.contains("WireGuard"))
            assertFalse(snippet.contains("DNS"))
            assertFalse(snippet.contains("HTTPS"))
        }
        assertTrue(statusPolicy.contains("AccountAccessPolicy.statusLine"))
        assertTrue(statusPolicy.contains("PENDING"))
    }
}

package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Screen-wiring guard for the "Подписка" surface (S5 §3.2A).
 *
 * Behavioural rules for the individual lines are covered by SubscriptionStatusTextTest,
 * SubscriptionTermTextTest, SubscriptionDeviceTextTest and SubscriptionPlanTextTest; this test
 * only asserts that the existing screen still binds those four policies to the accepted state
 * snapshot and stays on the existing destination, so it does not duplicate them. Assertions are
 * anchored on whole, meaningful bindings rather than arbitrary surrounding source substrings.
 */
class SubscriptionStatusSourceTest {
    private fun source(path: String): String = listOf(File(path), File("testapp/$path"))
        .first { it.isFile }.readText()

    private fun mainActivity(): String = source("src/main/java/xyz/terlimo/test/MainActivity.kt")

    @Test fun subscriptionScreenBindsAllStatusLinesToAcceptedState() {
        val activity = mainActivity()
        val render = activity.substringAfter("private fun render(")
        val subscriptionRender = render.substringAfter("subscription.text = SubscriptionStatusText.status(")

        assertTrue(
            "status line reads the accepted summary/account_access snapshot and the elapsed clock",
            subscriptionRender.startsWith(
                "\n            state.summary,\n            state.accountAccess,\n" +
                    "            android.os.SystemClock.elapsedRealtime(),\n        )",
            ),
        )
        assertTrue(
            "term line is derived from the same accepted entitlement projection and the purchase attempt",
            subscriptionRender.contains(
                "subscriptionTerm.text = state.accountAccess?.projection?.let {\n" +
                    "            SubscriptionTermText.term(it, state.purchase, java.time.ZoneId.systemDefault())\n" +
                    "        }.orEmpty()",
            ),
        )
        assertTrue(
            "device X-of-Y line is derived from the accepted account_access snapshot",
            subscriptionRender.contains(
                "subscriptionDevices.text = SubscriptionDeviceText.line(state.accountAccess).orEmpty()",
            ),
        )
        assertTrue(
            "paid/imported plan title is derived only from the accepted entitlement projection",
            subscriptionRender.contains(
                "subscriptionPlan.text = SubscriptionPlanText.line(state.accountAccess?.projection).orEmpty()",
            ),
        )
        listOf("subscriptionTerm", "subscriptionDevices", "subscriptionPlan").forEach { view ->
            assertTrue(
                "$view is hidden whenever its line is empty",
                subscriptionRender.contains(
                    "$view.visibility = if ($view.text.isNullOrEmpty()) View.GONE else View.VISIBLE",
                ),
            )
        }
    }

    @Test fun subscriptionScreenLivesOnExistingDestinationAndAddsNoSecondTab() {
        val activity = mainActivity()
        val creation = activity.substringAfter("subscription = TextView(this)")
            .substringBefore("subscriptionPanel.addView(TextView(this).apply { text = \"Оплата и продление\"")
        listOf("subscription", "subscriptionTerm", "subscriptionDevices", "subscriptionPlan").forEach { view ->
            assertTrue(
                "$view is added to the existing subscription panel",
                creation.contains("subscriptionPanel.addView($view)"),
            )
        }
        assertTrue(
            "the status view stays visible on the surface",
            creation.startsWith(".apply { setPadding(0, 16, 0, 16); visibility = View.VISIBLE }"),
        )
        val surface = activity.substringAfter("val subscriptionSurface = ScrollView(this)")
            .substringBefore("val helpSurface")
        assertTrue(surface.contains("subscriptionPanel.visibility = View.VISIBLE"))
        assertTrue(surface.contains("addView(subscriptionPanel)"))
        assertTrue(
            "the bottom navigation still declares the existing \"Подписка\" destination",
            source("src/main/java/xyz/terlimo/test/BottomNavigation.kt").contains(
                "NavDestination(\"Подписка\", R.drawable.ic_nav_subscription, NavTarget.SUBSCRIPTION)",
            ),
        )
        assertTrue(
            "that destination is bound to the existing subscription surface",
            activity.contains("NavTarget.SUBSCRIPTION -> subscriptionSurface"),
        )
        assertTrue(
            "the existing surface is part of the tab visibility model",
            activity.contains("val surfaces = listOf(mainSurface, subscriptionSurface, settingsSurface, helpSurface)"),
        )
    }

    @Test fun subscriptionStatusPolicyStaysTransportFree() {
        val activity = mainActivity()
        val statusPolicy = source("src/main/java/xyz/terlimo/test/SubscriptionStatusText.kt")
        val assignment = activity.substringAfter("subscription.text = SubscriptionStatusText.status(")
            .substringBefore("subscriptionTerm.text =")
        listOf(assignment, statusPolicy).forEach { snippet ->
            assertFalse(snippet.contains("WireGuard"))
            assertFalse(snippet.contains("DNS"))
            assertFalse(snippet.contains("HTTPS"))
        }
        assertTrue(statusPolicy.contains("AccountAccessPolicy.statusLine"))
        assertTrue(statusPolicy.contains("PENDING"))
    }
}

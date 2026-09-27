package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Source-shape of the display-only onboarding hour correction: the real main/notification
 * paths use the shared wording, the main surface reuses the existing status text, and the
 * refresh remains a main-thread, lifecycle-bound display tick (no network, no alarm) whose
 * arm/stop decision comes from the unit-tested AccountAccessDisplayRefresh.shouldPost.
 */
class OnboardingHourSourceTest {
    private fun source(path: String): String = listOf(File(path), File("testapp/$path"), File("../$path"))
        .first { it.isFile }.readText()

    private fun sha256(text: String): String =
        java.security.MessageDigest.getInstance("SHA-256").digest(text.toByteArray(Charsets.UTF_8))
            .joinToString("") { "%02x".format(it) }

    private val policy = "src/main/java/xyz/terlimo/test/AccountAccessPolicy.kt"
    private val refresh = "src/main/java/xyz/terlimo/test/AccountAccessDisplayRefresh.kt"
    private val service = "src/main/java/xyz/terlimo/test/SessionService.kt"
    private val activity = "src/main/java/xyz/terlimo/test/MainActivity.kt"

    @Test fun onboardingWordingReplacesTheTrialWordingInProductSources() {
        val text = source(policy)
        assertTrue(text.contains("Доступ для регистрации"))
        assertTrue(text.contains("скоро завершится"))
        assertTrue(text.contains("Час доступа завершён"))
        assertFalse(text.contains("Пробн"))

        listOf(activity, service, "src/main/java/xyz/terlimo/test/SubscriptionStatusText.kt")
            .forEach { assertFalse(it, source(it).contains("Пробн")) }
    }

    @Test fun screenAndNotificationUseTheNewPolicyThroughTheExistingPaths() {
        val activityText = source(activity)
        val render = activityText.substringAfter("private fun render(")
        assertTrue(render.contains("subscription.text = SubscriptionStatusText.status("))
        assertTrue(render.contains("state.accountAccess"))
        assertFalse(render.contains("Пробн"))

        // Main surface: the existing status text view carries the same display policy.
        val mainStatus = activityText.substringAfter("private fun mainStatusText(")
            .substringBefore("private fun pendingSelectionStatus(")
        assertTrue(mainStatus.contains("UserStatusText.phase"))
        assertTrue(mainStatus.contains("AccountAccessPolicy.statusLine"))
        assertTrue(mainStatus.contains("state.accountAccess"))
        listOf("WireGuard", "DNS", "HTTPS").forEach { assertFalse(it, mainStatus.contains(it)) }
        assertTrue(render.contains("status.text = mainStatusText(state)"))

        val notification = source(service).substringAfter("private fun updateForegroundState(")
            .substringBefore("override fun onBind")
        assertTrue(notification.contains(
            "AccountAccessPolicy.notificationLine(state.accountAccess, SystemClock.elapsedRealtime())"))
    }

    @Test fun refreshIsLifecycleBoundAndNeverPollsTheNetworkOrAlarms() {
        val serviceText = source(service)
        assertTrue(serviceText.contains("AccountAccessDisplayRefresh"))
        assertTrue(serviceText.contains("main.postDelayed(this, AccountAccessDisplayRefresh.TICK_MILLIS)"))
        assertTrue(serviceText.contains("main.postDelayed(accessTick, AccountAccessDisplayRefresh.TICK_MILLIS)"))
        assertTrue(serviceText.contains("refreshAccessDisplay(attempt)"))
        assertTrue(serviceText.contains("AccountAccessDisplayRefresh.shouldPost("))
        assertTrue(serviceText.substringAfter("private fun stopAttempt(").contains("main.removeCallbacks(accessTick)"))
        assertTrue(serviceText.substringAfter("override fun onDestroy()")
            .contains("main.removeCallbacksAndMessages(null)"))

        // Bound exactly the accessTick Runnable body. Traffic/usage ticks were later
        // inserted before the tunnel field and are not part of this display tick; scoping
        // to the next top-level field keeps the invariant on the real accessTick body.
        val tick = serviceText.substringAfter("private val accessTick = object : Runnable")
            .substringBefore("\n    private val ")
        assertTrue(tick.contains("updateForegroundState(view)"))
        assertTrue(tick.contains("AccountAccessDisplayRefresh.shouldPost("))
        listOf("AlarmManager", "Connectivity", "URL", "http", "stopAttempt", "publish").forEach {
            assertFalse(it, tick.contains(it))
        }

        val refreshText = source(refresh)
        assertTrue(refreshText.contains("AccountAccessPolicy.isConfirmedOnboardingHour"))
        listOf("AlarmManager", "ConnectivityManager", "URL", "http", "postDelayed").forEach {
            assertFalse(it, refreshText.contains(it))
        }

        val activityText = source(activity)
        val render = activityText.substringAfter("private fun render(")
        val activityTick = activityText.substringAfter("private val statusTick = object : Runnable")
            .substringBefore("override fun onCreate(")
        assertTrue(activityTick.contains("if (!statusTickStarted) return"))
        assertTrue(activityTick.contains("refreshStatusTexts()"))
        assertTrue(activityTick.contains("AccountAccessDisplayRefresh.shouldPost(statusTickStarted"))
        listOf("AlarmManager", "Connectivity", "URL", "http", "SessionService.start", "startForegroundService",
            "readCatalogCache", "publish").forEach {
            assertFalse(it, activityTick.contains(it))
        }

        // At most one queued callback: remove first, then a single conditional post.
        val arm = activityText.substringAfter("private fun armStatusTick()")
            .substringBefore("private fun refreshStatusTexts()")
        assertTrue(arm.contains("main.removeCallbacks(statusTick)"))
        assertTrue(arm.contains("AccountAccessDisplayRefresh.shouldPost(statusTickStarted"))
        assertTrue(arm.contains("main.postDelayed(statusTick, AccountAccessDisplayRefresh.TICK_MILLIS)"))
        assertEquals(1, Regex("postDelayed\\(").findAll(arm).count())

        assertTrue(activityText.substringAfter("override fun onStart()").contains("statusTickStarted = true"))
        val stop = activityText.substringAfter("override fun onStop()")
            .substringBefore("override fun onActivityResult")
        assertTrue(stop.contains("statusTickStarted = false"))
        assertTrue(stop.contains("main.removeCallbacks(statusTick)"))
        assertTrue(render.contains("armStatusTick()"))
    }

    @Test fun everyHourSurfaceUsesTheSharedBlockingPredicateWithoutCopyingAdmission() {
        val policyText = source(policy)

        val status = policyText.substringAfter("fun statusLine(")
            .substringBefore("fun isConfirmedOnboardingHour(")
        assertTrue(status.contains("onboardingHourBlockedReason(snapshot)"))
        assertTrue(status.contains("isConfirmedOnboardingHour("))

        val predicate = policyText.substringAfter("fun isConfirmedOnboardingHour(")
            .substringBefore("private fun accessText(")
        listOf("dataAccess == ONBOARDING_HOUR", "onboarding.state == \"active\"", "remainingMillis",
            "onboardingHourBlockedReason(snapshot) == null", "REVOKED_SESSION",
            "BLOCKING_BINDING_STATUSES", "\"revoked\"", "\"expired\"", "\"unknown_review\"").forEach {
            assertTrue(it, predicate.contains(it))
        }

        val notification = policyText.substringAfter("fun notificationLine(")
            .substringBefore("private const val ONBOARDING_HOUR")
        assertTrue(notification.contains("onboardingHourBlockedReason(snapshot) != null"))
        assertTrue(notification.contains("isConfirmedOnboardingHour("))

        // The display layer never re-implements native admission or infers catalog/grant
        // network health from the account snapshot.
        listOf("ValidUntil", "lease", "http", "URL", "Connectivity", "AlarmManager").forEach {
            assertFalse(it, policyText.contains(it))
        }

        // Main status, subscription status, notification line and tick; all through the
        // shared policy.
        assertTrue(source("src/main/java/xyz/terlimo/test/SubscriptionStatusText.kt")
            .contains("AccountAccessPolicy.statusLine"))
        assertTrue(source(refresh).contains("AccountAccessPolicy.isConfirmedOnboardingHour"))
        val activityText = source(activity)
        assertTrue(activityText.contains("AccountAccessPolicy.statusLine"))
        assertTrue(activityText.contains("AccountAccessDisplayRefresh.shouldPost"))
        val serviceText = source(service)
        assertTrue(serviceText.contains("AccountAccessPolicy.notificationLine"))
        assertTrue(serviceText.contains("AccountAccessDisplayRefresh.shouldPost"))
    }

    @Test fun acceptedNativeAdmissionStaysByteIdentical() {
        // a730b28 is accepted and must stay: this display correction only reuses its
        // prohibitions and never edits the native admission. Re-pin only after a native
        // admission review.
        val admission = source("go_client/accountaccess/admission.go")
        assertTrue(admission.contains("func confirmedActiveOnboardingHour(me MeResponse, now time.Time) bool"))
        assertEquals(ADMISSION_SHA256, sha256(admission))
    }

    @Test fun helpAndStatusSurfacesOfferNoTrialIssuanceAction() {
        val help = source(activity).substringAfter("val helpPanel = LinearLayout(this)")
            .substringBefore("subscriptionPanel.addView(TextView(this).apply { text = \"Подписка TERLIMO\"")
        assertTrue(help.contains("Помощь"))
        assertFalse(help.contains("Telegram"))
        // S3-A adds the explicit Telegram registration action; S3-B adds an explicit trial
        // action that must be gated by the server's can_activate signal (no unguarded issuance).
        assertTrue(source(activity).contains("Зарегистрироваться в Telegram"))
        assertTrue(source(activity).contains("TrialUi.activateVisible(state)"))

        val catalog = source("src/main/java/xyz/terlimo/test/ServerCatalogView.kt")
        assertFalse(catalog.contains("Telegram"))
    }

    private companion object {
        /** Byte-identity of the accepted native admission (a730b28; re-pin only after review). */
        const val ADMISSION_SHA256 = "fa92cb023322a6e68ef664eb2e8d0f7e952a479c1a8a1e99db94ba1b792ecbfa"
    }
}

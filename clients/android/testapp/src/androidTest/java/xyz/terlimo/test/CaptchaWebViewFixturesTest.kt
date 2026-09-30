package xyz.terlimo.test

import android.Manifest
import android.app.NotificationManager
import android.net.Uri
import android.os.Build
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith

/**
 * Controlled WebView fixtures for the ported official v20 managers: every page is a local
 * data: URL, so the flows auto success / slider fallback / manual success / manual cancel /
 * background notification / return / late are exercised without any VK traffic.
 * Runs on the phone gate after root review (no device access while the source is under review).
 */
@RunWith(AndroidJUnit4::class)
class CaptchaWebViewFixturesTest {

    private val context get() = InstrumentationRegistry.getInstrumentation().targetContext

    private fun dataUrl(html: String): String = "data:text/html;charset=utf-8," + Uri.encode(html)

    private fun fixture(body: String): String = """
        <!doctype html><html><head><meta name="viewport" content="width=device-width, initial-scale=1">
        <!-- not_robot_captcha -->
        </head><body>$body</body></html>
    """.trimIndent()

    private val checkboxSuccess = fixture(
        """<label class="vkc__Checkbox-module__Checkbox" for="not-robot-captcha-checkbox">
             <input type="checkbox" id="not-robot-captcha-checkbox"></label>
           <script>setTimeout(function(){ window.WdttCaptcha.onSuccess('fixture-token'); }, 250);</script>"""
    )

    private val sliderPage = fixture(
        """<div class="vkc__SliderCaptcha-module__description">Потяните ползунок</div>"""
    )

    private val manualSuccess = fixture(
        """<script>setTimeout(function(){ window.WdttCaptcha.onSuccess('manual-fixture-token'); }, 250);</script>"""
    )

    private val manualCancel = fixture(
        """<script>setTimeout(function(){ window.WdttCaptcha.onCancel(); }, 250);</script>"""
    )

    @Before fun armTunnelAndPermission() {
        CaptchaWebViewManager.onTunnelStart(context.applicationContext)
        AppForeground.isForeground = true
        if (Build.VERSION.SDK_INT >= 33) {
            runCatching {
                InstrumentationRegistry.getInstrumentation().uiAutomation
                    .grantRuntimePermission(context.packageName, Manifest.permission.POST_NOTIFICATIONS)
            }
        }
    }

    @Test fun autoSuccessReturnsTheFixtureToken() = runBlocking {
        val token = CaptchaWebViewManager.solveCaptchaAsync(dataUrl(checkboxSuccess), "fixture-session")
        assertEquals("fixture-token", token)
        CaptchaWebViewManager.onTunnelStop()
    }

    @Test fun sliderPageFailsWithTheOfficialSliderReason() = runBlocking {
        val failure = runCatching {
            CaptchaWebViewManager.solveCaptchaAsync(dataUrl(sliderPage), "fixture-session")
        }.exceptionOrNull()
        assertNotNull(failure)
        assertEquals(CaptchaWebViewManager.ERROR_SLIDER_DETECTED, failure?.message)
        CaptchaWebViewManager.onTunnelStop()
    }

    @Test fun manualWindowSolvesAndReportsExactlyOnce() = runBlocking {
        val token = ManlCaptchaWebViewManager.solveCaptchaAsync(context, dataUrl(manualSuccess), "fixture-session")
        assertEquals("manual-fixture-token", token)
        // Late/duplicate callback after completion is dropped by the official manager.
        ManlCaptchaWebViewManager.notifyResult(Result.success("late"))
        assertFalse(ManlCaptchaWebViewManager.isCaptchaPending)
    }

    @Test fun manualCancelReportsTheOfficialRefusal() = runBlocking {
        val failure = runCatching {
            ManlCaptchaWebViewManager.solveCaptchaAsync(context, dataUrl(manualCancel), "fixture-session")
        }.exceptionOrNull()
        assertNotNull(failure)
        assertTrue(failure?.message == "Cancelled by user")
    }

    @Test fun backgroundShowsNotificationAndReturnRelaunchesTheSameWindow() = runBlocking {
        AppForeground.isForeground = false
        val manager = context.getSystemService(NotificationManager::class.java)
        val job = launch {
            assertEquals("manual-fixture-token",
                ManlCaptchaWebViewManager.solveCaptchaAsync(context, dataUrl(manualSuccess), "fixture-session"))
        }
        // Background: the donor posts its high-importance notification instead of starting the window.
        var notified = false
        for (attempt in 0 until 40) {
            delay(100)
            if (manager.activeNotifications.any { it.id == 9001 }) { notified = true; break }
        }
        assertTrue("official CAPTCHA notification was not posted in background", notified)
        // Return to the app: exactly one pending window is relaunched.
        AppForeground.isForeground = true
        ManlCaptchaWebViewManager.checkAndShowPendingCaptcha(context)
        job.join()
        assertFalse(manager.activeNotifications.any { it.id == 9001 })
    }
}

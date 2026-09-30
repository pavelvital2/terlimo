package xyz.terlimo.test

import android.content.Intent
import android.os.SystemClock
import android.util.Log
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

/**
 * On-device service→UI→result/cancel path (synthetic CAPTCHA).
 *
 * The synthetic challenge is injected through the debug-only SessionService seam into the real
 * dispatcher/queue/service; the real ManlCaptchaActivity window is opened, the result is delivered
 * through the real manager callback, and the real Connect budget/watchdog is started for the
 * active attempt. This is NOT a live VK CAPTCHA and does not claim one: it proves the service/UI
 * wiring, correlation, the >15 s manual-wait guarantee and the Disconnect-during-wait fence.
 */
@RunWith(AndroidJUnit4::class)
class CaptchaServicePathTest {

    private val context get() = InstrumentationRegistry.getInstrumentation().targetContext

    private companion object {
        const val SYNTH_URL = "https://id.vk.ru/captcha/selfcheck-synthetic"
        const val TAG = "WDTT/Captcha"
    }

    private fun waitFor(timeoutMs: Long, what: String, condition: () -> Boolean) {
        val deadline = SystemClock.elapsedRealtime() + timeoutMs
        while (SystemClock.elapsedRealtime() < deadline) {
            if (condition()) return
            Thread.sleep(100)
        }
        throw AssertionError("timeout waiting for $what")
    }

    private fun ensureStopped() {
        if (SessionService.isRunning()) {
            context.startForegroundService(Intent(context, SessionService::class.java).setAction("cancel"))
        }
        waitFor(30_000, "service stopped") { !SessionService.isRunning() }
    }

    private fun ensureAttemptAndCatalog() {
        context.startActivity(Intent(context, MainActivity::class.java)
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
        // The cold Activity bootstrap is the explicit resume path; a bound-only instance has
        // no active attempt, so the test starts the same writer the UI cold start uses. A
        // transient background-start rejection is retried with diagnostics, not silently.
        val deadline = SystemClock.elapsedRealtime() + 60_000
        while (SystemClock.elapsedRealtime() < deadline &&
            !(SessionService.isRunning() && SessionService.debugAttemptId() != null)) {
            try {
                context.startForegroundService(Intent(context, SessionService::class.java).setAction("resume"))
            } catch (t: Throwable) {
                Log.w(TAG, "debug resume rejected: $t")
            }
            Thread.sleep(3_000)
        }
        Log.i(TAG, "debug attempt state running=${SessionService.isRunning()} " +
            "attempt=${SessionService.debugAttemptId()} phase=${SessionService.view.phase} " +
            "nodes=${SessionService.view.nodes.size} selected=${SessionService.view.selectedNodeId}")
        if (!(SessionService.isRunning() && SessionService.debugAttemptId() != null)) {
            throw AssertionError("no active attempt: running=${SessionService.isRunning()} " +
                "phase=${SessionService.view.phase} error=${SessionService.view.error}")
        }
        waitFor(60_000, "catalog ready") {
            SessionService.view.phase == "CatalogReady" && SessionService.view.nodes.isNotEmpty()
        }
    }

    @Test
    fun syntheticManualWaitSurvivesWatchdogAndResumesWithRemaining() {
        ensureStopped()
        ensureAttemptAndCatalog()
        val attempt = SessionService.debugAttemptId()!!
        assertTrue("origin contract must accept the synthetic URL", CaptchaOrigins.allowed(SYNTH_URL))

        assertTrue("debug budget start", SessionService.debugStartConnectBudget())
        val end0 = SessionService.debugBudgetEnd()
        assertTrue("real budget started", end0 != Long.MAX_VALUE)

        assertTrue("synthetic captcha injected",
            SessionService.debugInjectCaptcha("synth-manual-1", "manual", SYNTH_URL, "synth"))
        waitFor(15_000, "captcha pending + real window") {
            SessionService.captchaPending && ManlCaptchaWebViewManager.isCaptchaPending
        }
        assertEquals("pause must not move the deadline", end0, SessionService.debugBudgetEnd())
        assertFalse(SessionService.debugBudgetExpired())

        // Longer than the general 15 s Connect watchdog: the manual wait must not be aborted.
        Thread.sleep(17_000)
        assertTrue("manual wait was aborted by the general watchdog", SessionService.captchaPending)
        assertFalse("paused budget expired", SessionService.debugBudgetExpired())
        assertTrue("service must stay alive", SessionService.isRunning())
        assertEquals("attempt must stay active", attempt, SessionService.debugAttemptId())
        assertNotEquals("no premature watchdog terminal", "VPN_SETUP_TIMEOUT", SessionService.view.error)

        // Real manager callback → service queue → correlated result to native.
        ManlCaptchaWebViewManager.notifyResult(Result.success("synth-token"))
        waitFor(10_000, "completion") { !SessionService.captchaPending }
        val end1 = SessionService.debugBudgetEnd()
        assertTrue("budget must resume with the remaining time (end0=$end0 end1=$end1)",
            end1 - end0 >= 16_000)
        waitFor(10_000, "window closed") { ManlCaptchaWebViewManager.activeActivity == null }

        ensureStopped()
    }

    @Test
    fun disconnectDuringManualWaitClosesWindowAndDropsLateResult() {
        ensureStopped()
        ensureAttemptAndCatalog()
        assertTrue("synthetic captcha injected",
            SessionService.debugInjectCaptcha("synth-manual-2", "manual", SYNTH_URL, "synth"))
        waitFor(15_000, "captcha pending + real window") {
            SessionService.captchaPending && ManlCaptchaWebViewManager.activeActivity != null
        }

        context.startForegroundService(Intent(context, SessionService::class.java).setAction("cancel"))
        waitFor(15_000, "captcha torn down") { !SessionService.captchaPending }
        waitFor(15_000, "window closed") { ManlCaptchaWebViewManager.activeActivity == null }

        // A late UI callback of the cancelled prompt must not resurrect anything.
        ManlCaptchaWebViewManager.notifyResult(Result.success("late"))
        Thread.sleep(1_500)
        assertFalse(SessionService.captchaPending)
        assertNull(ManlCaptchaWebViewManager.activeActivity)
        waitFor(30_000, "service stopped") { !SessionService.isRunning() }
    }

    /**
     * Real Connect (bounded) for runtime HTTPS evidence: reads the product readiness snapshot the
     * app itself records (`lastReadiness`) after the tunnel passes DNS + HTTPS + HTTP status +
     * expected-exit-IP verification of the real request through the VPN route.
     */
    @Test
    fun realConnectReadinessProvidesRuntimeHttpsEvidence() {
        ensureStopped()
        ensureAttemptAndCatalog()
        val node = SessionService.view.nodes.first()
        context.startForegroundService(Intent(context, SessionService::class.java)
            .setAction("choose").putExtra("node_id", node.id))
        waitFor(20_000, "selection confirmed") { SessionService.view.selectedNodeId == node.id }
        val connectable = NodeSelection.connectableNodeId(SessionService.view.nodes,
            SessionService.view.selectedNodeId)
        Log.i(TAG, "debug connect node=$connectable")
        if (connectable == null) throw AssertionError("no connectable node after choose")
        // Let any in-flight manual refresh cycle settle: a revision change during admission is
        // fenced by the product as REVISION_CONFLICT, which is not a CAPTCHA-path failure.
        val revisionBefore = SessionService.view.catalogRevision
        Thread.sleep(2_500)
        if (SessionService.view.catalogRevision != revisionBefore) Thread.sleep(2_500)
        context.startForegroundService(Intent(context, SessionService::class.java)
            .setAction("select").putExtra("node_id", connectable))
        waitFor(120_000, "connected") { SessionService.view.phase == "Connected" }

        val readiness = SessionService.lastReadiness
        Log.i(TAG, "debug real readiness=$readiness")
        assertEquals("runtime readiness must pass", true, readiness["ready"])
        assertEquals("DNS through VPN", true, readiness["dnsOk"])
        assertEquals("HTTPS request through VPN", true, readiness["httpsOk"])
        assertEquals("HTTP status through VPN", true, readiness["httpStatusOk"])
        assertEquals("expected exit IP (route proven)", true, readiness["expectedExitOk"])

        context.startForegroundService(Intent(context, SessionService::class.java).setAction("cancel"))
        waitFor(30_000, "stopped") { !SessionService.isRunning() }
    }
}

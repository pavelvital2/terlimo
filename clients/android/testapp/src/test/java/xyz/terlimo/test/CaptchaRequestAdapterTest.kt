package xyz.terlimo.test

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Controlled CAPTCHA fixtures (no VK traffic): the official v20 dispatcher semantics,
 * error mapping, exactly-once/late/duplicate policy and the notification/return decisions.
 */
class CaptchaRequestAdapterTest {

    private class FakeSolver : CaptchaSolver {
        val calls = mutableListOf<String>()
        val autoQueue = ArrayDeque<() -> String>()
        var manualAnswer: () -> String = { "manual-token" }

        override suspend fun solveAuto(redirectUri: String, sessionToken: String, onStep: (String) -> Unit): String {
            calls += "auto"
            val answer = if (autoQueue.isNotEmpty()) autoQueue.removeFirst() else { { "auto-token" } }
            return answer()
        }

        override suspend fun solveManual(redirectUri: String, sessionToken: String): String {
            calls += "manual"
            return manualAnswer()
        }
    }

    private fun timeout(): TimeoutCancellationException = runBlocking {
        try {
            withTimeout(1) { delay(50) }
            error("unreachable")
        } catch (e: TimeoutCancellationException) {
            e
        }
    }

    @Test fun autoModeRunsOnlyTheBoundedAutoSolver() = runBlocking {
        val solver = FakeSolver()
        val adapter = CaptchaRequestAdapter(solver)
        assertEquals("auto-token", adapter.solve("auto", "https://vk.com/c", "s"))
        assertEquals(listOf("auto"), solver.calls)
    }

    @Test fun manualModeRunsOnlyTheManualWindow() = runBlocking {
        val solver = FakeSolver()
        val adapter = CaptchaRequestAdapter(solver)
        assertEquals("manual-token", adapter.solve("manual", "https://vk.com/c", "s"))
        assertEquals(listOf("manual"), solver.calls)
    }

    @Test fun selectedDefaultRetriesAutoThenFallsBackToManual() = runBlocking {
        val solver = FakeSolver()
        solver.autoQueue += { throw timeout() } // first attempt times out
        val adapter = CaptchaRequestAdapter(solver, { "auto" })
        assertEquals("auto-token", adapter.solve("selected", "https://vk.com/c", "s"))
        assertEquals(listOf("auto", "auto"), solver.calls)
    }

    @Test fun selectedDefaultOpensManualAfterTwoAutoTimeouts() = runBlocking {
        val solver = FakeSolver()
        solver.autoQueue += { throw timeout() }
        solver.autoQueue += { throw timeout() }
        val adapter = CaptchaRequestAdapter(solver, { "auto" })
        assertEquals("manual-token", adapter.solve("selected", "https://vk.com/c", "s"))
        assertEquals(listOf("auto", "auto", "manual"), solver.calls)
    }

    @Test fun selectedOpensManualImmediatelyForSlider() = runBlocking {
        val solver = FakeSolver()
        solver.autoQueue += { throw IllegalStateException(CaptchaRequestAdapter.ERROR_SLIDER_DETECTED) }
        val adapter = CaptchaRequestAdapter(solver, { "auto" })
        assertEquals("manual-token", adapter.solve("selected", "https://vk.com/c", "s"))
        assertEquals(listOf("auto", "manual"), solver.calls)
    }

    @Test fun selectedRespectsTheChosenManualMethod() = runBlocking {
        val solver = FakeSolver()
        val adapter = CaptchaRequestAdapter(solver, { "manual" })
        assertEquals("manual-token", adapter.solve("selected", "https://vk.com/c", "s"))
        assertEquals(listOf("manual"), solver.calls)
    }

    @Test fun unknownModeFollowsTheOfficialSelectedBranch() = runBlocking {
        val solver = FakeSolver()
        val adapter = CaptchaRequestAdapter(solver, { "manual" })
        assertEquals("manual-token", adapter.solve("something-else", "https://vk.com/c", "s"))
        assertEquals(listOf("manual"), solver.calls)
    }

    @Test fun selectedOtherStateFailurePropagatesWithItsReason() = runBlocking {
        val solver = FakeSolver()
        solver.autoQueue += { throw IllegalStateException("captcha_check_error") }
        val adapter = CaptchaRequestAdapter(solver, { "auto" })
        val failure = runCatching { adapter.solve("selected", "https://vk.com/c", "s") }.exceptionOrNull()
        assertTrue(failure is IllegalStateException)
        assertEquals("error:captcha_check_error", adapter.errorValue(failure!!))
        assertEquals(listOf("auto"), solver.calls)
    }

    @Test fun errorMappingMatchesTheOfficialHandler() {
        val adapter = CaptchaRequestAdapter(FakeSolver())
        assertEquals("error:timeout", adapter.errorValue(timeout()))
        assertEquals("error:cancelled", adapter.errorValue(CancellationException("tunnel stopped")))
        assertEquals("error:slider_detected", adapter.errorValue(IllegalStateException("slider_detected")))
        assertEquals("error:WV state error", adapter.errorValue(IllegalStateException()))
        assertEquals("error:VK: boom", adapter.errorValue(Exception("VK: boom")))
        assertEquals("error:Exception", adapter.errorValue(Exception()))
    }

    @Test fun pendingPolicyFixtureBackgroundNotificationAndReturnLaunch() {
        assertTrue(CaptchaPendingPolicy.shouldNotify(isForeground = false))
        assertFalse(CaptchaPendingPolicy.shouldNotify(isForeground = true))
        assertTrue(CaptchaPendingPolicy.shouldStartActivity(isForeground = true))
        assertFalse(CaptchaPendingPolicy.shouldStartActivity(isForeground = false))
        // Return to the app: exactly one pending window is relaunched while none is active.
        assertTrue(CaptchaPendingPolicy.shouldRelaunchPending(intentPending = true, activityActive = false))
        assertFalse(CaptchaPendingPolicy.shouldRelaunchPending(intentPending = true, activityActive = true))
        assertFalse(CaptchaPendingPolicy.shouldRelaunchPending(intentPending = false, activityActive = false))
    }

    @Test fun originsAllowOnlyTheCaptchaHostsOverHttps() {
        assertTrue(CaptchaOrigins.allowed("https://id.vk.ru/captcha?x=1"))
        assertTrue(CaptchaOrigins.allowed("https://vk.com/not_robot_captcha"))
        assertFalse(CaptchaOrigins.allowed("http://vk.com/captcha"))
        assertFalse(CaptchaOrigins.allowed("https://evil.example/captcha"))
        assertFalse(CaptchaOrigins.allowed("https://vk.com.evil.example/captcha"))
        assertFalse(CaptchaOrigins.allowed("https://user@vk.com/captcha"))
        assertFalse(CaptchaOrigins.allowed("https://vk.com:8443/captcha"))
    }
}

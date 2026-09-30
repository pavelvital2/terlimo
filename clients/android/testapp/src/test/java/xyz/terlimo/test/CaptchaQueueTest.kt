package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/** Service adapter policy: exactly-once, stale, cancel and fast-new-attempt fences. */
class CaptchaQueueTest {

    private fun entry(attempt: String = "a1", id: String = "r1") =
        CaptchaQueue.Entry(attempt, id, "auto", "https://vk.com/c", "s")

    @Test fun requestIsAcceptedOnceAndCompletedExactlyOnce() {
        val queue = CaptchaQueue()
        assertEquals(CaptchaQueue.Decision.ACCEPT, queue.request("a1", "r1"))
        queue.activate(entry())
        assertEquals(CaptchaQueue.Decision.IGNORE_DUPLICATE, queue.request("a1", "r1"))
        val completed = queue.complete("a1", "r1")
        assertEquals("r1", completed?.id)
        assertNull("second completion is dropped", queue.complete("a1", "r1"))
        assertEquals("late duplicate of a completed id is ignored",
            CaptchaQueue.Decision.IGNORE_DUPLICATE, queue.request("a1", "r1"))
    }

    @Test fun differentRequestWhileActiveIsBusyAndNeverTouchesThePrompt() {
        val queue = CaptchaQueue()
        queue.request("a1", "r1")
        queue.activate(entry())
        assertEquals(CaptchaQueue.Decision.BUSY, queue.request("a1", "r2"))
        assertEquals(CaptchaQueue.Decision.BUSY, queue.request("a2", "r1"))
        assertEquals("r1", queue.activeEntry()?.id)
    }

    @Test fun cancelThenFastNewAttemptDropsTheOldCompletion() {
        val queue = CaptchaQueue()
        queue.request("a1", "r1")
        queue.activate(entry(attempt = "a1", id = "r1"))
        queue.invalidate() // Disconnect/stop of attempt a1

        // Fast new attempt with its own prompt.
        assertEquals(CaptchaQueue.Decision.ACCEPT, queue.request("a2", "r2"))
        queue.activate(entry(attempt = "a2", id = "r2"))

        assertNull("old completion must not clear the new prompt", queue.complete("a1", "r1"))
        assertEquals("r2", queue.activeEntry()?.id)
        assertEquals("r2", queue.complete("a2", "r2")?.id)
    }

    @Test fun lateCallbackAfterCancelIsDropped() {
        val queue = CaptchaQueue()
        queue.request("a1", "r1")
        queue.activate(entry())
        queue.invalidate()
        assertNull(queue.complete("a1", "r1"))
        assertNull(queue.activeEntry())
        // Background notification callback for the dead prompt: still no active entry.
        assertNull(queue.complete("a1", "r1"))
    }
}

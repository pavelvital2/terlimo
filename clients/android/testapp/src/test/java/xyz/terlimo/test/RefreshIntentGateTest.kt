package xyz.terlimo.test

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class RefreshIntentGateTest {
    private val window = 300L

    @Test
    fun `first tap is accepted`() {
        assertTrue(RefreshIntentGate.accept(null, 1_000L, window))
    }

    @Test
    fun `tap inside the platform double-tap window is suppressed`() {
        assertFalse(RefreshIntentGate.accept(1_000L, 1_200L, window))
        assertFalse(RefreshIntentGate.accept(1_000L, 1_300L, window))
    }

    @Test
    fun `deliberate tap after the window is accepted and a short cycle still guards`() {
        assertTrue(RefreshIntentGate.accept(1_000L, 1_301L, window))
        // A very short native cycle does not weaken the guard: the window is the only rule.
        assertFalse(RefreshIntentGate.accept(1_000L, 1_050L, window))
        assertTrue(RefreshIntentGate.accept(1_000L, 1_500L, window))
    }
}

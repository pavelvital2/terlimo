package xyz.terlimo.test

/**
 * Host-side double-tap guard for the manual refresh action (S5 §11/§29). A second refresh
 * intent that arrives inside the platform double-tap window is a duplicate of the user's
 * first tap and must not forward another native request. The window is the platform
 * [android.view.ViewConfiguration.getDoubleTapTimeout] passed in by the caller; the clock
 * is the monotonic [android.os.SystemClock.elapsedRealtime] value, never wall time. This
 * only coalesces identical user taps: outside the window a deliberate new tap is accepted,
 * and the native single-flight fence still protects an in-flight cycle.
 */
internal object RefreshIntentGate {
    fun accept(lastAcceptedMs: Long?, nowMs: Long, windowMs: Long): Boolean =
        lastAcceptedMs == null || nowMs - lastAcceptedMs > windowMs
}

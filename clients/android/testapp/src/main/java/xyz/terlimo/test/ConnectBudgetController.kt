package xyz.terlimo.test

/**
 * Pairs the Connect budget with its single watchdog. Pausing for a bounded CAPTCHA wait
 * removes the watchdog; resuming re-arms it with exactly the remaining budget. A stale
 * completion of another attempt can never pause, resume or re-arm the current one.
 */
internal class ConnectBudgetController(
    private val deadline: ConnectDeadline,
    private val cancelWatchdog: () -> Unit,
    private val armWatchdog: (attempt: String, delayMillis: Long) -> Unit,
) {
    fun pause(attempt: String, now: Long): Long? {
        val remaining = deadline.pause(attempt, now) ?: return null
        cancelWatchdog()
        return remaining
    }

    fun resume(attempt: String, now: Long): Long? {
        val remaining = deadline.resume(attempt, now) ?: return null
        armWatchdog(attempt, remaining)
        return remaining
    }
}

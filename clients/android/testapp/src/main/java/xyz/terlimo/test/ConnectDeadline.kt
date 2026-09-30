package xyz.terlimo.test

/**
 * A single user Connect owns one budget, including queued work and readiness.
 * The budget can be paused while a bounded official CAPTCHA wait is in flight: the user
 * wait never consumes the network budget, and the operation continues afterwards with
 * exactly the remaining budget. Disconnect/expiry clear the budget as before.
 */
internal class ConnectDeadline {
    private var attempt: String? = null
    private var end = Long.MAX_VALUE
    private var paused = false
    private var pausedRemaining = 0L

    @Synchronized fun start(id: String, now: Long): Boolean {
        if (attempt != null) return false
        attempt = id
        end = now + MILLIS
        paused = false
        pausedRemaining = 0L
        return true
    }
    @Synchronized fun deadline(id: String): Long = if (attempt == id) end else Long.MAX_VALUE
    @Synchronized fun expired(id: String, now: Long): Boolean = attempt == id && !paused && now >= end
    @Synchronized fun complete(id: String, now: Long): Boolean {
        if (attempt != id) return true // Recovery uses its existing outer budget.
        if (!paused && now >= end) return false
        clear()
        return true
    }
    /** Pauses the budget; returns the remaining time or null when this attempt is not the owner. */
    @Synchronized fun pause(id: String, now: Long): Long? {
        if (attempt != id || paused) return null
        pausedRemaining = (end - now).coerceAtLeast(0L)
        paused = true
        return pausedRemaining
    }
    /** Resumes the budget with exactly the remaining time; null when not paused for this attempt. */
    @Synchronized fun resume(id: String, now: Long): Long? {
        if (attempt != id || !paused) return null
        paused = false
        end = now + pausedRemaining
        return pausedRemaining
    }
    @Synchronized fun clear() { attempt = null; end = Long.MAX_VALUE; paused = false; pausedRemaining = 0L }

    companion object { const val MILLIS = 15_000L }
}

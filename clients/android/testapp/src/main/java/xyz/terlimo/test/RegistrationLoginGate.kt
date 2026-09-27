package xyz.terlimo.test

internal enum class RegistrationLoginTapAction { SEND_NOW, START_SERVICE, IGNORE, ERROR }

/**
 * Explicit "already have an account -> sign in via Telegram" gate for a fresh installation
 * (no onboarding-hour right). It reuses the existing registration link/confirm flow: one
 * bounded service-only attempt is started when no attempt is live, and exactly one link
 * request is sent after the first accepted /me on it. No new API, right or hour is created.
 */
internal class RegistrationLoginGate {
    private var startPending = false
    private var coldAttempt: String? = null
    private var sentAttempt: String? = null

    @Synchronized
    fun onTap(attemptId: String?, eligible: Boolean): RegistrationLoginTapAction {
        if (startPending || coldAttempt != null || sentAttempt != null) return RegistrationLoginTapAction.IGNORE
        if (!eligible) return RegistrationLoginTapAction.ERROR
        if (attemptId == null) {
            startPending = true
            return RegistrationLoginTapAction.START_SERVICE
        }
        sentAttempt = attemptId
        return RegistrationLoginTapAction.SEND_NOW
    }

    @Synchronized
    fun onAttemptStarted(attemptId: String): Boolean {
        if (!startPending) return false
        startPending = false
        coldAttempt = attemptId
        return true
    }

    @Synchronized
    fun isCold(attemptId: String?): Boolean = coldAttempt != null && coldAttempt == attemptId

    /** First accepted /me on the cold attempt: send exactly one link request. */
    @Synchronized
    fun onVerifiedRights(attemptId: String): Boolean {
        if (coldAttempt != attemptId || sentAttempt != null) return false
        sentAttempt = attemptId
        return true
    }

    /** The link request reached a pending/error result: release own cold attempt once. */
    @Synchronized
    fun onResolved(attemptId: String?): Boolean {
        if (attemptId == null || sentAttempt != attemptId) return false
        val wasCold = coldAttempt == attemptId
        clear()
        return wasCold
    }

    @Synchronized
    fun onTimeout(attemptId: String?): Boolean {
        if (startPending && attemptId == null) {
            clear()
            return true
        }
        if (coldAttempt != null && coldAttempt == attemptId) {
            clear()
            return true
        }
        return false
    }

    @Synchronized
    fun reset() = clear()

    private fun clear() {
        startPending = false
        coldAttempt = null
        sentAttempt = null
    }
}

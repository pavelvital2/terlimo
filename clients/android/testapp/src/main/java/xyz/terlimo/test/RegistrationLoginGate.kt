package xyz.terlimo.test

internal enum class RegistrationLoginTapAction { SEND_NOW, START_SERVICE, IGNORE, ERROR }
internal enum class RegistrationAction(val wireType: String) {
    LOGIN("request_telegram_registration"),
    REGISTER("request_telegram_registration"),
    REFRESH("refresh_telegram_registration");

    fun eligible(state: ViewState): Boolean = when (this) {
        LOGIN -> RegistrationUi.loginVisible(state)
        REGISTER -> RegistrationUi.registerVisible(state)
        REFRESH -> state.registration?.state == "pending"
    }
}
internal sealed interface RegistrationVerified {
    data class Send(val action: RegistrationAction) : RegistrationVerified
    data object Refused : RegistrationVerified
    data object Ignore : RegistrationVerified
}

/** One explicit action, one bounded service-only attempt; never owns an existing VPN. */
internal class RegistrationLoginGate {
    private var startPending = false
    private var coldAttempt: String? = null
    private var sentAttempt: String? = null
    private var action: RegistrationAction? = null
    private var recovering = false
    private var sequence = 0L

    @Synchronized
    fun onTap(attemptId: String?, requested: RegistrationAction, eligible: Boolean): RegistrationLoginTapAction {
        if (action != null) return RegistrationLoginTapAction.IGNORE
        if (!eligible) return RegistrationLoginTapAction.ERROR
        sequence++
        action = requested
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

    /** Refresh remains read-only even if the fresh /me already says registered or none. */
    @Synchronized
    fun onVerifiedRights(attemptId: String, eligible: (RegistrationAction) -> Boolean): RegistrationVerified {
        if (coldAttempt != attemptId || sentAttempt != null) return RegistrationVerified.Ignore
        val requested = action ?: return RegistrationVerified.Ignore
        sentAttempt = attemptId
        if (requested != RegistrationAction.REFRESH && !eligible(requested)) return RegistrationVerified.Refused
        return RegistrationVerified.Send(requested)
    }

    @Synchronized
    fun expects(attemptId: String, refresh: Boolean): Boolean = sentAttempt == attemptId && !recovering &&
        (action == RegistrationAction.REFRESH) == refresh

    /** Retain only our own cold attempt for a same-owner, known-order status recovery. */
    @Synchronized
    fun beginRecovery(attemptId: String): Boolean {
        if (!expects(attemptId, true)) return false
        recovering = true
        return true
    }

    @Synchronized
    fun finishRecovery(attemptId: String): Boolean = if (recovering) onResolved(attemptId) else false

    @Synchronized
    fun onResolved(attemptId: String?): Boolean {
        if (attemptId == null || sentAttempt != attemptId) return false
        val wasCold = coldAttempt == attemptId
        clear()
        return wasCold
    }

    @Synchronized
    fun token(): Long = sequence

    @Synchronized
    fun onTimeout(attemptId: String?, token: Long = sequence): Boolean {
        if (token != sequence) return false
        if ((startPending && attemptId == null) ||
            (attemptId != null && (coldAttempt == attemptId || sentAttempt == attemptId))) {
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
        action = null
        recovering = false
    }
}

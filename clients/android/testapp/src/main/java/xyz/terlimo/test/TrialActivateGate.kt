package xyz.terlimo.test

/**
 * S3-B explicit trial activation action gate.
 *
 * The trial action must work even when there is no live VPN/data attempt (after the hour,
 * in KillSwitch hold or Idle): a bounded service-only attempt is started through the
 * existing linkless `begin("")` path and `activate_trial` is sent exactly once, only after
 * a fresh confirmed `/me` (registration confirmed, trial.can_activate) on that attempt.
 *
 * The gate is a pure state machine so the action/coalescing/cancellation rules are testable
 * without the Android service. It never derives eligibility and never bypasses the server.
 */
internal enum class TrialTapAction { SEND_NOW, START_SERVICE, IGNORE, ERROR }

internal class TrialActivateGate {
    private var startPending = false
    private var coldAttempt: String? = null
    private var sentAttempt: String? = null
    private var coldConsumed = false

    /** One explicit tap. `attemptId` is the current active attempt or null. */
    @Synchronized
    fun onTap(attemptId: String?, canActivate: Boolean): TrialTapAction {
        if (startPending || coldAttempt != null || sentAttempt != null) return TrialTapAction.IGNORE
        if (attemptId == null) {
            startPending = true
            return TrialTapAction.START_SERVICE
        }
        if (!canActivate) return TrialTapAction.ERROR
        sentAttempt = attemptId
        return TrialTapAction.SEND_NOW
    }

    /** A new attempt started; records it as the cold service-only purpose when requested. */
    @Synchronized
    fun onAttemptStarted(attemptId: String): Boolean {
        if (!startPending) return false
        startPending = false
        coldAttempt = attemptId
        coldConsumed = false
        return true
    }

    /** Fresh accepted /me rights. Returns true only for the first confirmed cold attempt. */
    @Synchronized
    fun onVerifiedRights(attemptId: String, registrationConfirmed: Boolean, canActivate: Boolean): Boolean {
        if (coldAttempt != attemptId || coldConsumed) return false
        if (!registrationConfirmed || !canActivate) return false
        coldConsumed = true
        sentAttempt = attemptId
        return true
    }

    /** Terminal trial result. Returns true when the cold service-only attempt must be stopped. */
    @Synchronized
    fun onTerminal(attemptId: String): Boolean {
        val stop = coldAttempt == attemptId
        clear()
        return stop
    }

    /** Bounded timeout for a cold attempt that never produced a confirmed /me. */
    @Synchronized
    fun onTimeout(attemptId: String?): Boolean {
        if (coldAttempt == null || coldAttempt != attemptId || coldConsumed) return false
        clear()
        return true
    }

    /** Control path could not start: clear and surface a fixed error, never a silent no-op. */
    @Synchronized
    fun onControlFailed(): Boolean {
        val failed = startPending || coldAttempt != null
        clear()
        return failed
    }

    @Synchronized
    fun reset() { clear() }

    private fun clear() {
        startPending = false
        coldAttempt = null
        sentAttempt = null
        coldConsumed = false
    }
}

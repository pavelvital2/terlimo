package xyz.terlimo.test

/**
 * STEP03.6 explicit-Connect / onboarding-hour gate.
 *
 * The pre-admission onboarding-hour intent may only be requested from a true user
 * Connect: the `select` action issued after the mandatory VPN consent and the
 * one-tap retained connect funnel. Resume, import, enrollment, background/wake
 * recovery, switch and probe never arm it.
 *
 * This is a pure attempt-fenced state holder: it starts no work, performs no I/O,
 * and does not change admission or lifecycle behavior. The native side remains the
 * single owner of the actual intent/start flow.
 */
object ExplicitConnectGate {
    enum class Entry(val explicit: Boolean) {
        SELECT(true),
        ONE_TAP_CONNECT(true),
        // Pre-admission first connect: the fresh-install Connect after the mandatory
        // VPN consent, before any node selection or catalogue exists.
        PRE_ADMISSION(true),
        RESUME(false),
        IMPORT(false),
        BACKGROUND(false),
        RECOVERY(false),
        WAKE(false),
        SWITCH(false),
        PROBE(false),
    }

    fun arms(entry: Entry): Boolean = entry.explicit
}

/** Attempt-fenced arming state: a stale attempt can never keep the gate armed. */
class ExplicitConnectArming {
    private var armedAttempt: String? = null

    fun arm(entry: ExplicitConnectGate.Entry, attempt: String?): Boolean {
        if (attempt.isNullOrEmpty() || !ExplicitConnectGate.arms(entry)) return false
        armedAttempt = attempt
        return true
    }

    fun armed(attempt: String?): Boolean = attempt != null && armedAttempt == attempt

    fun clear() {
        armedAttempt = null
    }
}

package xyz.terlimo.test

/** Explicit catalogue expectation phase of the CURRENT attempt. */
internal enum class MobileCatalogState { IDLE, WAITING_RIGHT, WAITING_CATALOG, CATALOG_ACCEPTED }

/**
 * Bounded action the caller applies to the finite catalogue window timer. There is deliberately
 * no terminal/off action: a restricted_checkout grant keeps its checkout/control meaning and
 * never becomes a global stop.
 */
internal enum class MobileCatalogAction { ARM, DISARM, NONE }

/**
 * Per-attempt catalogue expectation state machine (mobile account_access lifecycle).
 *
 * Exactly one state belongs to the CURRENT attempt:
 * - WAITING_RIGHT: no catalogue timer (no valid data right yet); the attempt stays alive;
 * - WAITING_CATALOG: one finite 15s timer is armed; repeated accepted data rights must not
 *   move/reset/extend it;
 * - CATALOG_ACCEPTED: terminal for the attempt and tracked independently of the timer; it
 *   never re-arms.
 *
 * Every event is fenced by the attempt id: stale events for an old attempt return NONE and can
 * neither arm, disarm nor commit anything for the current attempt. Non-mobile (legacy) attempts
 * keep the begin/catalog timer lifecycle and never react to account_access rights.
 */
internal class MobileCatalogGate {
    private var attempt: String? = null
    private var mobile = false
    private var state = MobileCatalogState.IDLE

    /** Begins a new attempt: the finite catalogue window is armed before any child start. */
    @Synchronized
    fun onAttemptStart(attempt: String, mobile: Boolean): MobileCatalogAction {
        this.attempt = attempt
        this.mobile = mobile
        state = MobileCatalogState.WAITING_CATALOG
        return MobileCatalogAction.ARM
    }

    /**
     * One accepted CURRENT mobile projection. Leaving WAITING_CATALOG for a no-data right
     * disarms; entering WAITING_CATALOG from WAITING_RIGHT arms exactly once for that
     * transition. Unknown rights and repeated accepted rights change nothing.
     */
    @Synchronized
    fun onAccountAccess(dataAccess: String, attempt: String): MobileCatalogAction {
        if (attempt != this.attempt || !mobile) return MobileCatalogAction.NONE
        return when (dataAccess) {
            "none", "restricted_checkout" -> {
                if (state != MobileCatalogState.WAITING_CATALOG) MobileCatalogAction.NONE
                else {
                    state = MobileCatalogState.WAITING_RIGHT
                    MobileCatalogAction.DISARM
                }
            }
            "onboarding_hour", "subscription_data" -> {
                if (state != MobileCatalogState.WAITING_RIGHT) MobileCatalogAction.NONE
                else {
                    state = MobileCatalogState.WAITING_CATALOG
                    MobileCatalogAction.ARM
                }
            }
            else -> MobileCatalogAction.NONE
        }
    }

    /** Fresh catalogue accepted: terminal for this attempt, the timer is disarmed. */
    @Synchronized
    fun onCatalogAccepted(attempt: String): MobileCatalogAction {
        if (attempt != this.attempt || state == MobileCatalogState.CATALOG_ACCEPTED) return MobileCatalogAction.NONE
        state = MobileCatalogState.CATALOG_ACCEPTED
        return MobileCatalogAction.DISARM
    }

    /** Resets the current attempt and fences all older callbacks; a stale stop changes nothing. */
    @Synchronized
    fun onAttemptStop(attempt: String?): MobileCatalogAction {
        if (attempt != null && attempt != this.attempt) return MobileCatalogAction.NONE
        this.attempt = null
        mobile = false
        state = MobileCatalogState.IDLE
        return MobileCatalogAction.DISARM
    }

    /** Read-only probe of the state for [attempt] (IDLE for anything but the current attempt). */
    @Synchronized
    fun stateOf(attempt: String): MobileCatalogState =
        if (attempt == this.attempt) state else MobileCatalogState.IDLE
}

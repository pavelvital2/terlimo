package xyz.terlimo.test

/**
 * Display-only refresh cadence for the visible onboarding hour. It never gates, stops,
 * admits or extends anything: native remains the stop owner. While the accepted snapshot
 * still shows the onboarding hour with time remaining, the existing render/notification
 * path re-runs on a main-thread Handler tick so the frozen-clock countdown does not
 * freeze between native events. No network, no alarm, no permission.
 */
internal object AccountAccessDisplayRefresh {
    /** The visible text carries seconds, so one display tick is one second. */
    const val TICK_MILLIS = 1_000L

    /**
     * The single (re)arm decision of the display tick, shared by the main screen and the
     * notification. The tick may be (re)posted only while the owner is started and the
     * accepted snapshot still carries a confirmed active onboarding hour with time left.
     * A late callback after onStop (lifecycleStarted == false), a null/rejected snapshot,
     * a paid/trial/checkout/none right, a blocking snapshot prohibition (revoked
     * session/binding/entitlement) or the expiry instant all stop it: there is no
     * background or perpetual polling.
     */
    fun shouldPost(lifecycleStarted: Boolean, snapshot: AccountAccessSnapshot?, nowElapsed: Long): Boolean =
        lifecycleStarted && remains(snapshot, nowElapsed)

    /** True while an accepted snapshot shows the confirmed onboarding hour with time remaining. */
    fun remains(snapshot: AccountAccessSnapshot?, nowElapsed: Long): Boolean =
        snapshot != null && AccountAccessPolicy.isConfirmedOnboardingHour(snapshot, nowElapsed)
}

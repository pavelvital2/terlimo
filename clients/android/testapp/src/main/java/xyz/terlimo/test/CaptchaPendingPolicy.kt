package xyz.terlimo.test

/** Process-wide foreground state used by the donor notification/launch decisions. */
internal object AppForeground {
    @Volatile var isForeground: Boolean = false
}

/**
 * Donor v20 manual-window decisions, extracted so the controlled fixtures can prove them
 * without a device: notify only when the app is not foreground, launch the activity only
 * while it is, and relaunch exactly one pending window when the user returns.
 */
internal object CaptchaPendingPolicy {
    fun shouldNotify(isForeground: Boolean): Boolean = !isForeground
    fun shouldStartActivity(isForeground: Boolean): Boolean = isForeground
    fun shouldRelaunchPending(intentPending: Boolean, activityActive: Boolean): Boolean =
        intentPending && !activityActive
}

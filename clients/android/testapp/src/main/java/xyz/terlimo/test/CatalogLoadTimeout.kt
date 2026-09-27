package xyz.terlimo.test

/**
 * Catalogue-acquisition deadline lifecycle for one attempt.
 *
 * The 15s catalogue deadline belongs only to an unfinished catalogue acquisition. It is armed
 * when the attempt begins and permanently disarmed the moment a fresh catalogue is accepted for
 * that same attempt. Later phases (NodeAuthenticating/ConfiguringVPN/readiness/switching) never
 * re-arm it, so an old deadline callback cannot stop an accepted direct reconnect.
 *
 * Pure Kotlin (no Android API) so the lifecycle is directly unit-testable. Attempt fencing is
 * the caller's responsibility (`gate.active == attempt`); this holder only answers whether the
 * deadline is still pending for a given attempt.
 */
internal class CatalogDeadlineLifecycle {
    @Volatile private var armedFor: String? = null

    fun arm(attempt: String) { armedFor = attempt }

    /** Disarms only if the caller's attempt is the one currently armed. */
    fun disarm(attempt: String) { if (armedFor == attempt) armedFor = null }

    fun clear() { armedFor = null }

    fun pending(attempt: String): Boolean = armedFor == attempt
}

internal object CatalogLoadTimeout {
    const val MILLIS = 15_000L

    /** Stop only an attempt that is still active AND still awaiting its catalogue. */
    fun shouldStop(activeAttempt: String?, expectedAttempt: String, catalogPending: Boolean): Boolean =
        activeAttempt == expectedAttempt && catalogPending
}

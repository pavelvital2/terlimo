package xyz.terlimo.test

/**
 * Finite catalogue window timer for the single active attempt.
 *
 * Wraps the existing [CatalogDeadlineLifecycle] / [CatalogLoadTimeout] machinery: every applied
 * ARM schedules exactly one bounded 15s callback with its own monotonic window id, so a newer
 * arm fences every older callback out and a fresh window is never cut short. The commit asks
 * the host for the active attempt plus the lifecycle pending flag, so a disarmed, accepted or
 * superseded attempt never stops.
 *
 * [CatalogTimerMarkerRecord] emissions are secret-safe observation only: they carry the marker
 * kind, an attempt correlation id, monotonic elapsed milliseconds since the ARM and a UTC
 * millisecond stamp, never an account id, key, URL or private catalogue. Emission never changes
 * the timer decision.
 */
internal enum class CatalogTimerMarker { ARM, DISARM, ACCEPT, TIMEOUT }

internal data class CatalogTimerMarkerRecord(
    val marker: CatalogTimerMarker,
    val attempt: String,
    val elapsedMs: Long,
    val utcMs: Long,
    val reason: String? = null,
    val revision: String? = null,
)

internal class CatalogDeadlineTimer(
    private val schedule: (delayMillis: Long, callback: () -> Unit) -> Unit,
    private val activeAttempt: () -> String?,
    private val onTimeout: (attempt: String) -> Unit,
    private val elapsed: () -> Long = { 0L },
    private val utc: () -> Long = { 0L },
    private val emit: (CatalogTimerMarkerRecord) -> Unit = {},
) {
    private val deadline = CatalogDeadlineLifecycle()
    @Volatile private var window = 0L
    @Volatile private var armAttempt: String? = null
    @Volatile private var armElapsed: Long = 0L

    fun apply(
        attempt: String,
        action: MobileCatalogAction,
        marker: CatalogTimerMarker? = null,
        detail: String? = null,
    ) {
        when (action) {
            MobileCatalogAction.ARM -> arm(attempt)
            MobileCatalogAction.DISARM -> {
                // Observation only: a DISARM/ACCEPT marker describes an actually pending
                // window for this attempt. A stale/duplicate action (no pending timer) knows
                // nothing and emits nothing; the underlying lifecycle disarm still runs
                // exactly as before so timer decisions are unchanged.
                val pending = deadline.pending(attempt)
                deadline.disarm(attempt)
                if (pending && marker != null) {
                    if (marker == CatalogTimerMarker.ACCEPT) emitMarker(marker, attempt, null, detail)
                    else emitMarker(marker, attempt, detail, null)
                }
                if (pending && armAttempt == attempt) armAttempt = null
            }
            MobileCatalogAction.NONE -> Unit
        }
    }

    private fun arm(attempt: String) {
        deadline.arm(attempt)
        armAttempt = attempt
        armElapsed = elapsed()
        emitMarker(CatalogTimerMarker.ARM, attempt, null, null)
        window++
        val armedWindow = window
        schedule(CatalogLoadTimeout.MILLIS) {
            if (armedWindow == window &&
                CatalogLoadTimeout.shouldStop(activeAttempt(), attempt, deadline.pending(attempt))) {
                emitMarker(CatalogTimerMarker.TIMEOUT, attempt, null, null)
                if (armAttempt == attempt) armAttempt = null
                onTimeout(attempt)
            }
        }
    }

    fun clear(reason: String? = null) {
        // A clear with no pending window is not a catalogue disarm: never emit a second,
        // misleading DISARM after ACCEPT/TIMEOUT or for an already disarmed attempt.
        val attempt = armAttempt
        val pending = attempt != null && deadline.pending(attempt)
        deadline.clear()
        if (pending && attempt != null) emitMarker(CatalogTimerMarker.DISARM, attempt, reason ?: "clear", null)
        armAttempt = null
    }

    fun pending(attempt: String): Boolean = deadline.pending(attempt)

    private fun emitMarker(marker: CatalogTimerMarker, attempt: String, reason: String?, revision: String?) {
        val delta = if (marker == CatalogTimerMarker.ARM) 0L else (elapsed() - armElapsed).coerceAtLeast(0L)
        emit(CatalogTimerMarkerRecord(marker, attempt, delta, utc(), reason, revision))
    }
}

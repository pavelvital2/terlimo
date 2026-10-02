package xyz.terlimo.test

/**
 * Finite catalogue window timer for the single active attempt.
 *
 * Wraps the existing [CatalogDeadlineLifecycle] / [CatalogLoadTimeout] machinery: every applied
 * Legacy ARM schedules one bounded 15s callback; mobile beginStages uses 20/25/10/10s
 * with a 65s active-processing cap, excluding only owned bounded CAPTCHA waits.
 * Each callback with its own monotonic window id, so a newer
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

internal data class CatalogCaptchaOwner(val attempt: String, val cycle: String, val request: String)

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
    private val onStageTimeout: (String, CatalogStage) -> Unit = { attempt, _ -> onTimeout(attempt) },
) {
    private val deadline = CatalogDeadlineLifecycle()
    private val stages = CatalogStages()
    @Volatile private var window = 0L
    @Volatile private var armAttempt: String? = null
    @Volatile private var armElapsed: Long = 0L
    private var captchaOwner: CatalogCaptchaOwner? = null
    private val captchaRequests = mutableSetOf<String>()

    @Synchronized fun apply(
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
                if (stages.attempt == attempt) {
                    stages.clear()
                    captchaOwner = null
                    captchaRequests.clear()
                    window++
                }
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
        captchaOwner = null
        captchaRequests.clear()
        stages.clear()
        deadline.arm(attempt)
        armAttempt = attempt
        armElapsed = elapsed()
        emitMarker(CatalogTimerMarker.ARM, attempt, null, null)
        window++
        val armedWindow = window
        schedule(CatalogLoadTimeout.MILLIS) {
            synchronized(this) {
                if (armedWindow == window &&
                    CatalogLoadTimeout.shouldStop(activeAttempt(), attempt, deadline.pending(attempt))) {
                    emitMarker(CatalogTimerMarker.TIMEOUT, attempt, null, null)
                    if (armAttempt == attempt) armAttempt = null
                    onTimeout(attempt)
                }
            }
        }
    }

    @Synchronized fun clear(reason: String? = null) {
        // A clear with no pending window is not a catalogue disarm: never emit a second,
        // misleading DISARM after ACCEPT/TIMEOUT or for an already disarmed attempt.
        val attempt = armAttempt
        val pending = attempt != null && deadline.pending(attempt)
        deadline.clear()
        stages.clear()
        captchaOwner = null
        captchaRequests.clear()
        window++
        if (pending && attempt != null) emitMarker(CatalogTimerMarker.DISARM, attempt, reason ?: "clear", null)
        armAttempt = null
    }

    @Synchronized fun beginStages(attempt: String, cycle: String) {
        if (stages.attempt == attempt && stages.cycle == cycle) return
        captchaOwner = null
        captchaRequests.clear()
        stages.begin(attempt, cycle, elapsed())
        deadline.arm(attempt)
        armAttempt = attempt; armElapsed = elapsed()
        emitMarker(CatalogTimerMarker.ARM, attempt, null, null)
        scheduleStage(attempt)
    }

    @Synchronized fun advanceStage(attempt: String, cycle: String, stage: CatalogStage): Boolean {
        if (!stages.advance(attempt, cycle, stage, elapsed())) return false
        scheduleStage(attempt)
        return true
    }

    private fun scheduleStage(attempt: String) {
        window++
        val armedWindow = window
        schedule(stages.remaining(elapsed())) {
            synchronized(this) {
                if (armedWindow == window && activeAttempt() == attempt && deadline.pending(attempt)) {
                    val stage = stages.stage ?: return@schedule
                    emitMarker(CatalogTimerMarker.TIMEOUT, attempt, null, null)
                    deadline.disarm(attempt); stages.clear(); captchaOwner = null; armAttempt = null
                    captchaRequests.clear()
                    onStageTimeout(attempt, stage)
                }
            }
        }
    }

    @Synchronized fun canAccept(attempt: String, cycle: String): Boolean =
        activeAttempt() == attempt && deadline.pending(attempt) && stages.canAccept(attempt, cycle, elapsed())

    @Synchronized fun pending(attempt: String): Boolean = deadline.pending(attempt)

    @Synchronized fun pauseCaptcha(owner: CatalogCaptchaOwner): Boolean {
        if (owner.request.isEmpty() || activeAttempt() != owner.attempt || !deadline.pending(owner.attempt)) return false
        captchaOwner?.let { return it == owner }
        if (owner.request in captchaRequests) return false
        if (!stages.pause(owner.attempt, owner.cycle, elapsed())) return false
        captchaOwner = owner
        captchaRequests += owner.request
        window++ // fence the already scheduled stage callback
        return true
    }

    @Synchronized fun resumeCaptcha(owner: CatalogCaptchaOwner): Boolean {
        if (captchaOwner != owner || activeAttempt() != owner.attempt || !deadline.pending(owner.attempt)) return false
        if (!stages.resume(elapsed())) return false
        captchaOwner = null
        scheduleStage(owner.attempt)
        return true
    }

    private fun emitMarker(marker: CatalogTimerMarker, attempt: String, reason: String?, revision: String?) {
        val delta = if (marker == CatalogTimerMarker.ARM) 0L else (elapsed() - armElapsed).coerceAtLeast(0L)
        emit(CatalogTimerMarkerRecord(marker, attempt, delta, utc(), reason, revision))
    }
}

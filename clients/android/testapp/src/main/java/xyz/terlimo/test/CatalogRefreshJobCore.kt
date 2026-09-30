package xyz.terlimo.test

/**
 * §26.5 per-run state machine of [CatalogRefreshJobService]. Pure: it never touches Android,
 * so the cold-process/VPN-off bound-only path, the active-service path, takeovers, Off,
 * disconnects, timeouts and onStopJob are all exercised in JVM tests.
 *
 * The job holds a bound-service lease only; it never starts a foreground service. Exactly one
 * [Finish] may be taken per run, and [onStopJob] ends the run without ever finishing the job.
 */
internal class CatalogRefreshJobCore(private val deadlineMillis: Long = DEFAULT_DEADLINE_MILLIS) {

    internal enum class Failure { REJECTED, BIND_FAILED, DISCONNECTED, TIMEOUT, CYCLE_ERROR, SCHEDULE_OFF }

    internal data class Finish(val ok: Boolean, val failure: Failure?, val error: String?)

    private var phase = Phase.IDLE
    private var requestId: String? = null
    private var finish: Finish? = null

    private enum class Phase { IDLE, BINDING, WAITING, FINISHED }

    val deadline: Long get() = deadlineMillis

    /** Returns true when the run must bind to the service; false ends it (Off or bad state). */
    fun start(scheduleEnabled: Boolean): Boolean {
        if (phase != Phase.IDLE) return false
        if (!scheduleEnabled) {
            phase = Phase.FINISHED
            return false
        }
        phase = Phase.BINDING
        requestId = "catalog-job-" + System.nanoTime()
        return true
    }

    fun requestId(): String? = requestId

    fun onServiceConnected(): Boolean {
        if (phase != Phase.BINDING) return false
        phase = Phase.WAITING
        return true
    }

    fun onRequestRejected() = finish(false, Failure.REJECTED, "rejected")

    fun onBindFailed() = finish(false, Failure.BIND_FAILED, "bind_failed")

    fun onServiceDisconnected() = finish(false, Failure.DISCONNECTED, "disconnected")

    fun onCycleResult(ok: Boolean, error: String?) =
        finish(ok, if (ok) null else Failure.CYCLE_ERROR, error)

    fun onTimeout() = finish(false, Failure.TIMEOUT, "timeout")

    fun onScheduleOff() = finish(false, Failure.SCHEDULE_OFF, "schedule_off")

    /** onStopJob: release the lease; the job is never finished from here. */
    fun onStopJob() {
        phase = Phase.FINISHED
        requestId = null
        finish = null
    }

    /** Exactly-once completion. Returns null when the run was stopped or already finished. */
    fun takeFinish(): Finish? {
        val result = finish ?: return null
        finish = null
        phase = Phase.FINISHED
        return result
    }

    private fun finish(ok: Boolean, failure: Failure?, error: String?) {
        if (phase == Phase.FINISHED || finish != null) return
        finish = Finish(ok, failure, error)
    }

    companion object {
        /** Bounded run window. Catalog cycles are naturally bounded by the existing 15s
         * catalog gate and attempt timeouts; this only caps the job-held lease. It never
         * extends a product timeout. */
        const val DEFAULT_DEADLINE_MILLIS = 90_000L
    }
}

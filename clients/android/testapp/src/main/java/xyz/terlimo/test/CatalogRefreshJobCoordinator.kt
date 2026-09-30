package xyz.terlimo.test

/**
 * §26.5 per-run glue between [CatalogRefreshJobService] and the bound writer. Pure Kotlin so
 * the ordering that matters on a real device is executable in JVM tests: two consecutive runs
 * on one Service instance, late callbacks of an old run after a new run started, onStopJob for
 * a mismatched JobParameters, bind failures/exceptions and token-scoped timeouts.
 *
 * A run is identified by a monotonically increasing token; every callback carries its token and
 * an event for a stale token is ignored. The actions interface is implemented by the Service.
 */
internal class CatalogRefreshJobCoordinator(private val actions: Actions) {

    internal interface Actions {
        fun bind(token: Long): Boolean
        fun unbind(token: Long)
        fun requestRefresh(token: Long, requestId: String, epochAtStart: Long): Boolean
        fun cancelRefresh(token: Long, requestId: String)
        fun finish(token: Long)
        fun discard(token: Long)
    }

    private class Run(val token: Long, val paramsTag: Any, val epochAtStart: Long) {
        val core = CatalogRefreshJobCore()
        var bound = false
        var cancellationSent = false
    }

    private var active: Run? = null

    /** Returns true when the run must stay alive (system should keep the job). */
    fun onStartJob(token: Long, paramsTag: Any, scheduleEnabled: Boolean, epochAtStart: Long): Boolean {
        abandonActive()
        val run = Run(token, paramsTag, epochAtStart)
        active = run
        if (!run.core.start(scheduleEnabled)) {
            active = null
            return false
        }
        run.bound = actions.bind(token)
        if (!run.bound) {
            // A synchronous bind failure means the work never started: consume the failure and
            // return false without calling jobFinished (the system sees no ongoing work).
            run.core.onBindFailed()
            run.core.takeFinish()
            release(run)
            active = null
            return false
        }
        return true
    }

    fun onServiceConnected(token: Long) {
        val run = current(token) ?: return
        if (!run.core.onServiceConnected()) return
        val requestId = run.core.requestId() ?: return
        if (!actions.requestRefresh(token, requestId, run.epochAtStart)) run.core.onRequestRejected()
        finishIfNeeded(run)
    }

    fun onCallback(token: Long, requestId: String, ok: Boolean, error: String?) {
        val run = current(token) ?: return
        if (run.core.requestId() != requestId) return
        run.core.onCycleResult(ok, error)
        finishIfNeeded(run)
    }

    fun onTimeout(token: Long) {
        val run = current(token) ?: return
        cancel(run)
        run.core.onTimeout()
        finishIfNeeded(run)
    }

    fun onScheduleOff(token: Long) {
        val run = current(token) ?: return
        cancel(run)
        run.core.onScheduleOff()
        finishIfNeeded(run)
    }

    fun onServiceLost(token: Long) {
        val run = current(token) ?: return
        run.core.onServiceDisconnected()
        finishIfNeeded(run)
    }

    fun onBindingFailed(token: Long) {
        val run = current(token) ?: return
        run.core.onBindFailed()
        finishIfNeeded(run)
    }

    /** Only the run that owns [paramsTag] is stopped; a stale params tag is ignored. */
    fun onStopJob(paramsTag: Any) {
        val run = active?.takeIf { it.paramsTag === paramsTag } ?: return
        cancel(run)
        run.core.onStopJob()
        release(run)
        actions.discard(run.token)
        active = null
    }

    fun onDestroy() = abandonActive()

    private fun abandonActive() {
        val run = active ?: return
        cancel(run)
        run.core.onStopJob()
        release(run)
        actions.discard(run.token)
        active = null
    }

    /** Returns true while the run is still waiting; false when it was finished or abandoned. */
    private fun finishIfNeeded(run: Run): Boolean {
        val finish = run.core.takeFinish()
        if (finish == null) {
            if (run.core.requestId() == null && active === run) {
                // Abandoned (onStopJob) while a late event arrived: ensure lease release.
                release(run)
                active = null
                return false
            }
            return true
        }
        release(run)
        if (active === run) active = null
        actions.finish(run.token)
        return false
    }

    private fun cancel(run: Run) {
        if (run.cancellationSent) return
        val requestId = run.core.requestId() ?: return
        run.cancellationSent = true
        actions.cancelRefresh(run.token, requestId)
    }

    private fun release(run: Run) {
        // Includes disconnect/rejection/success, before dropping the retained binder.
        cancel(run)
        if (run.bound) {
            run.bound = false
            actions.unbind(run.token)
        }
    }

    private fun current(token: Long): Run? = active?.takeIf { it.token == token }
}

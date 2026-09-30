package xyz.terlimo.test

import android.app.job.JobParameters
import android.app.job.JobService
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.ServiceConnection
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import java.util.concurrent.atomic.AtomicLong

/**
 * §26.5 periodic catalog refresh. The job holds a bound-service lease to the single catalog
 * writer (SessionService) and never starts a service or foreground service itself: a cold
 * process creates the service in bound-only mode (no FGS, no VPN notification) and it is
 * destroyed when the lease is released. A user/UI path that promotes the same instance keeps it
 * running; the job release never stops it.
 *
 * Every run is one [CatalogRefreshRunStore.Run]: own token, JobParameters, connection, binder
 * and timeout. Cleanup is idempotent and ordered (cancel the request through the retained
 * binder first, then unbind/remove), stale bind callbacks are refused, and jobFinished is sent
 * at most once and only while the run is still active.
 */
class CatalogRefreshJobService : JobService(), CatalogRefreshJobCoordinator.Actions {

    private val coordinator = CatalogRefreshJobCoordinator(this)
    private val runs = CatalogRefreshRunStore()
    private val sequence = AtomicLong(0)
    private val handler = Handler(Looper.getMainLooper())

    override fun onStartJob(jobParameters: JobParameters): Boolean {
        CatalogRefreshScheduleState.restore(this)
        val existingTag = runs.tokenFor(jobParameters)
        if (existingTag != null) return false
        val token = sequence.incrementAndGet()
        runs.start(token, jobParameters)
        val ongoing = coordinator.onStartJob(
            token = token,
            paramsTag = jobParameters,
            scheduleEnabled = CatalogRefreshScheduleState.mode() != CatalogRefreshMode.OFF,
            epochAtStart = CatalogRefreshScheduleState.currentEpoch(),
        )
        if (ongoing) {
            val timeout = Runnable { coordinator.onTimeout(token) }
            runs.run(token)?.timeout = timeout
            handler.postDelayed(timeout, CatalogRefreshJobCore.DEFAULT_DEADLINE_MILLIS)
        } else {
            cleanup(token)
        }
        return ongoing
    }

    override fun onStopJob(jobParameters: JobParameters?): Boolean {
        val tag = jobParameters ?: return false
        val token = runs.tokenFor(tag) ?: return false
        // The coordinator cancels through the retained binder and unbinds, without finishing.
        coordinator.onStopJob(tag)
        cleanup(token)
        return false
    }

    override fun onDestroy() {
        coordinator.onDestroy()
        super.onDestroy()
    }

    override fun discard(token: Long) = cleanup(token)

    // ---- CatalogRefreshJobCoordinator.Actions (main thread) ----

    override fun bind(token: Long): Boolean {
        val connection = object : ServiceConnection {
            override fun onServiceConnected(name: ComponentName?, service: IBinder?) {
                val local = service as? SessionService.LocalBinder ?: run {
                    coordinator.onBindingFailed(token)
                    return
                }
                // A late callback of an old run must never install its binder anywhere.
                if (!runs.setBinder(token, local)) return
                coordinator.onServiceConnected(token)
            }

            override fun onServiceDisconnected(name: ComponentName?) = coordinator.onServiceLost(token)
            override fun onBindingDied(name: ComponentName?) = coordinator.onServiceLost(token)
            override fun onNullBinding(name: ComponentName?) = coordinator.onBindingFailed(token)
        }
        val bound = try {
            bindService(Intent(this, SessionService::class.java), connection, Context.BIND_AUTO_CREATE)
        } catch (_: SecurityException) {
            false
        } catch (_: IllegalArgumentException) {
            false
        }
        if (!bound) return false
        return runs.bind(token, connection) != null
    }

    override fun unbind(token: Long) {
        val run = runs.run(token) ?: return
        if (run.bound) {
            run.connection?.let { runCatching { unbindService(it as ServiceConnection) } }
        }
        runs.clearBinding(token)
    }

    override fun requestRefresh(token: Long, requestId: String, epochAtStart: Long): Boolean {
        val run = runs.run(token) ?: return false
        val binder = run.binder as? SessionService.LocalBinder ?: return false
        val observer = object : CatalogRefreshObserver {
            override fun onCompleted(requestId: String, ok: Boolean, error: String?) {
                coordinator.onCallback(token, requestId, ok, error)
            }
        }
        return binder.requestCatalogRefresh(requestId, epochAtStart, observer)
    }

    override fun cancelRefresh(token: Long, requestId: String) {
        val binder = runs.run(token)?.binder as? SessionService.LocalBinder ?: return
        binder.cancelCatalogRefresh(requestId)
    }

    override fun finish(token: Long) {
        val run = runs.markFinished(token) ?: return
        run.timeout?.let(handler::removeCallbacks)
        cleanup(token)
        jobFinished(run.paramsTag as JobParameters, false)
    }

    /** Idempotent per-run cleanup: remove timer, unbind, forget the run. */
    private fun cleanup(token: Long) {
        val run = runs.end(token) ?: return
        run.timeout?.let(handler::removeCallbacks)
        if (run.bound) {
            run.connection?.let { runCatching { unbindService(it as ServiceConnection) } }
        }
    }
}

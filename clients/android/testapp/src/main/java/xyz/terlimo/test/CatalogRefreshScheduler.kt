package xyz.terlimo.test

import android.app.job.JobInfo
import android.app.job.JobScheduler
import android.content.ComponentName
import android.content.Context

/**
 * §26.5 JobScheduler glue: exactly one periodic entry, network-constrained, persisted so it
 * survives reboot (RECEIVE_BOOT_COMPLETED only re-schedules this catalog job; there is no VPN
 * boot receiver and no auto Connect). Reconciling is idempotent so Activity recreation never
 * restarts the next period. Schedule errors are reported, never masked as success.
 */
internal object CatalogRefreshScheduler {

    internal data class Result(val decision: CatalogRefreshPolicy.Decision, val scheduleResult: Int?)

    private fun scheduler(context: Context): JobScheduler =
        context.getSystemService(JobScheduler::class.java)

    private fun pending(context: Context): JobInfo? = scheduler(context).getPendingJob(CatalogRefreshPolicy.JOB_ID)

    fun pendingIntervalMillis(context: Context): Long? =
        pending(context)?.let { if (it.intervalMillis > 0) it.intervalMillis else null }

    fun reconcile(context: Context, desired: CatalogRefreshMode = CatalogRefreshScheduleState.mode()): Result {
        val decision = CatalogRefreshPolicy.reconcile(
            pendingInterval = pendingIntervalMillis(context),
            desired = desired,
        )
        return when (decision) {
            CatalogRefreshPolicy.Decision.UNCHANGED -> Result(decision, null)
            CatalogRefreshPolicy.Decision.CANCEL -> {
                scheduler(context).cancel(CatalogRefreshPolicy.JOB_ID)
                Result(decision, null)
            }
            CatalogRefreshPolicy.Decision.SCHEDULE -> {
                val interval = CatalogRefreshPolicy.intervalMillis(desired)!!
                val job = JobInfo.Builder(
                    CatalogRefreshPolicy.JOB_ID,
                    ComponentName(context, CatalogRefreshJobService::class.java),
                )
                    .setRequiredNetworkType(JobInfo.NETWORK_TYPE_ANY)
                    .setPersisted(true)
                    .setPeriodic(interval)
                    .build()
                Result(decision, scheduler(context).schedule(job))
            }
        }
    }
}

package xyz.terlimo.test

import android.content.Context

/**
 * §26.5 single serialization boundary for the periodic refresh lifecycle.
 *
 * One monitor owns everything that can change while a cycle is in flight:
 * - the persisted schedule mode and its monotonic epoch (Off/mode change is a linear event:
 *   [setMode] runs inside the same lock as [commit], so an epoch change can never be split
 *   from the publish/persist block it is supposed to invalidate);
 * - the ownership records of attempts and their periodic job requests;
 * - the admission/epoch captured when a job request is registered (a later attach never
 *   rewrites it to the then-current epoch).
 *
 * The actor remains the only thread that touches native/attempt state; all decisions that
 * gate that work happen here first, under the lock, so a queued begin can be invalidated by a
 * cancellation or Off that happened before it ran.
 */
internal class CatalogRefreshCoordinator {

    internal enum class Origin { MANUAL, JOB }

    internal data class Completion(val requestId: String, val ok: Boolean, val error: String?)

    internal data class ScheduleChange(
        val mode: CatalogRefreshMode,
        val epoch: Long,
        val jobFailures: List<Completion>,
        val attemptsToStop: List<String>,
        val applied: Boolean,
    )

    internal data class CancelPlan(val found: Boolean, val attemptsToStop: List<String>)

    internal data class CommitPlan(
        val publish: Boolean,
        val stopCycle: Boolean,
        val duplicate: Boolean,
        val completions: List<Completion>,
    )

    private data class Request(val id: String, val epoch: Long, var canceled: Boolean = false)

    private class AttemptRecord(val origin: Origin) {
        var manual = false
        var completedJobOnly = false
        val jobs = LinkedHashMap<String, Request>()
    }

    private var mode = CatalogRefreshMode.OFF
    private var epoch = 0L
    private var persistMode: ((CatalogRefreshMode) -> Unit)? = null
    private var manualIntent = false
    private var pendingCold: Request? = null
    private val requests = LinkedHashMap<String, Request>()
    private val attempts = LinkedHashMap<String, AttemptRecord>()

    /** Restores the persisted mode and arms persistence for later changes. */
    @Synchronized fun restore(context: Context) {
        val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        mode = CatalogRefreshMode.fromStored(prefs.getString(KEY_MODE, null))
        persistMode = { next ->
            prefs.edit().putString(KEY_MODE, next.stored).apply()
        }
    }

    @Synchronized fun mode(): CatalogRefreshMode = mode

    /** Tests only: forget all runtime state (the persisted prefs are not touched). */
    @Synchronized fun resetForTest() {
        mode = CatalogRefreshMode.OFF
        epoch = 0
        persistMode = null
        manualIntent = false
        pendingCold = null
        requests.clear()
        attempts.clear()
    }

    @Synchronized fun currentEpoch(): Long = epoch

    /**
     * Linear Off/mode change. Runs fully under the lock (persist + epoch + invalidation) and
     * returns only after the change is applied, so a UI caller never sees a false "off" before
     * the linear point. A concurrent [commit] either finished completely before this call or
     * starts completely after it.
     */
    @Synchronized fun setMode(next: CatalogRefreshMode): ScheduleChange {
        if (next == mode) return ScheduleChange(mode, epoch, emptyList(), emptyList(), applied = false)
        mode = next
        epoch += 1
        persistMode?.invoke(next)
        val failures = mutableListOf<Completion>()
        // Includes admitted requests whose actor task has not attached yet.
        requests.values.forEach { failures += Completion(it.id, false, "schedule_off") }
        requests.clear()
        pendingCold = null
        val toStop = mutableListOf<String>()
        attempts.forEach { (attempt, record) ->
            record.jobs.clear()
            if (record.origin == Origin.JOB && !record.manual && record.jobs.isEmpty()) toStop += attempt
        }
        return ScheduleChange(next, epoch, failures, toStop, applied = true)
    }

    /** A started-service intent is a live manual/UI owner of the cycle it will begin. */
    @Synchronized fun markServiceIntent() {
        manualIntent = true
    }

    @Synchronized fun markAttemptManual(attempt: String?) {
        if (attempt == null) return
        val record = attempts.getOrPut(attempt) { AttemptRecord(Origin.MANUAL) }
        record.manual = true
    }

    /**
     * Registers a periodic request with the epoch observed at job start. Rejected when the
     * schedule is Off or the job was started before the current epoch (stale dispatch).
     */
    @Synchronized fun registerJob(id: String, epochAtStart: Long): Boolean {
        if (mode == CatalogRefreshMode.OFF) return false
        if (epochAtStart != epoch) return false
        if (requests.containsKey(id)) return false
        requests[id] = Request(id, epoch)
        return true
    }

    @Synchronized fun pendCold(id: String): Boolean {
        val request = requests[id] ?: return false
        if (request.canceled || request.epoch != epoch) return false
        if (pendingCold != null) return false
        pendingCold = request
        return true
    }

    /** Attach uses the epoch captured at registration; never the then-current epoch. */
    @Synchronized fun attach(attempt: String, id: String): Boolean {
        val request = requests[id] ?: return false
        if (mode == CatalogRefreshMode.OFF || request.canceled || request.epoch != epoch) return false
        val record = attempts[attempt] ?: return false
        if (record.completedJobOnly && !record.manual) return false
        record.jobs[id] = request
        return true
    }

    /** Check the request and hand it to native in one order with cancel/Off. */
    @Synchronized fun dispatch(attempt: String, id: String, action: () -> Boolean): Boolean {
        if (!attach(attempt, id)) return false
        return action()
    }

    @Synchronized fun beginCycle(attempt: String, requiredJob: String? = null): Origin? {
        // A cold job may be canceled during bootstrap preparation. It must not become a
        // manual cycle when its pending claim disappears in that interval.
        if (requiredJob != null && !isColdJobValid(requiredJob)) return null
        val pending = pendingCold
        val origin = if (pending != null && !manualIntent) Origin.JOB else Origin.MANUAL
        val record = attempts.getOrPut(attempt) { AttemptRecord(origin) }
        if (manualIntent) record.manual = true
        if (pending != null && pending.epoch == epoch && !pending.canceled) record.jobs[pending.id] = pending
        pendingCold = null
        manualIntent = false
        return record.origin
    }

    /**
     * Actor-side validation of a queued cold dispatch: a cancellation or Off that happened
     * before the queued task ran invalidates it here.
     */
    @Synchronized fun isColdJobValid(id: String): Boolean {
        val pending = pendingCold ?: return false
        return pending.id == id && !pending.canceled && pending.epoch == epoch
    }

    @Synchronized fun failPendingCold(reason: String): Completion? {
        val pending = pendingCold ?: return null
        pendingCold = null
        requests.remove(pending.id)
        return Completion(pending.id, false, reason)
    }

    @Synchronized fun isJobOnly(attempt: String): Boolean {
        val record = attempts[attempt] ?: return false
        return record.origin == Origin.JOB && !record.manual
    }

    /**
     * Atomically decides and, when published, executes the publish/persist block inside the
     * lock. Records survive until [planTerminal]: a repeated job-only catalog event before the
     * attempt is torn down is a duplicate (publish=false, duplicate=true) and can never turn a
     * manual-less cycle into a publishing one.
     */
    @Synchronized fun commit(attempt: String, commitBlock: () -> Unit): CommitPlan {
        val record = attempts[attempt]
        if (record == null) {
            // Foreign/unknown attempt: keep the legacy manual behavior (never suppress).
            commitBlock()
            return CommitPlan(true, false, false, emptyList())
        }
        val valid = record.jobs.values.filter { !it.canceled && it.epoch == epoch }
        val completions = valid.map { Completion(it.id, true, null) }
        // A completed request is forgotten here: a later Off must not re-fail a job that already
        // succeeded, and a duplicate event must not publish for the same request again.
        valid.forEach { request ->
            record.jobs.remove(request.id)
            requests.remove(request.id)
        }
        return when {
            record.manual || record.origin == Origin.MANUAL -> {
                commitBlock()
                CommitPlan(true, false, false, completions)
            }
            record.completedJobOnly -> CommitPlan(false, false, true, emptyList())
            record.origin == Origin.JOB && valid.isEmpty() -> {
                record.completedJobOnly = true
                CommitPlan(false, true, false, emptyList())
            }
            else -> {
                record.completedJobOnly = true
                commitBlock()
                CommitPlan(true, false, false, completions)
            }
        }
    }

    /** Attempt ended: report failures to valid job requests and forget the record. */
    @Synchronized fun planTerminal(attempt: String, code: String): List<Completion> {
        val record = attempts.remove(attempt) ?: return emptyList()
        val completions = record.jobs.values
            .filter { !it.canceled && it.epoch == epoch }
            .map { Completion(it.id, false, code) }
        record.jobs.keys.forEach { requests.remove(it) }
        return completions
    }

    /** onStopJob / explicit release of one request. */
    @Synchronized fun cancelJob(id: String): CancelPlan {
        val request = requests[id]
        var found = false
        if (request != null) {
            request.canceled = true
            found = true
        }
        if (pendingCold?.id == id) {
            pendingCold = null
        }
        val toStop = mutableListOf<String>()
        attempts.forEach { (attempt, record) ->
            val removed = record.jobs.remove(id) != null
            if (removed) found = true
            if (removed && record.origin == Origin.JOB && !record.manual && record.jobs.isEmpty() &&
                !record.completedJobOnly) {
                toStop += attempt
            }
        }
        requests.remove(id)
        return CancelPlan(found, toStop)
    }

    private companion object {
        const val PREFS = "catalog_refresh"
        const val KEY_MODE = "mode"
    }
}

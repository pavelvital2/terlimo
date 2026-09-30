package xyz.terlimo.test

/**
 * §26.5 per-run resources of [CatalogRefreshJobService]. One run owns one entry: its token,
 * JobParameters tag, ServiceConnection, binder and timeout. All access is main-thread; the
 * class exists so the wrapper lifecycle (late callbacks, stop ordering, idempotent cleanup,
 * failed bind, false start) is exercised by JVM tests instead of only counter mocks.
 */
internal class CatalogRefreshRunStore {

    internal class Run(val token: Long, val paramsTag: Any) {
        var connection: Any? = null
        var binder: Any? = null
        var bound = false
        var finished = false
        var timeout: Runnable? = null
    }

    private val runs = HashMap<Long, Run>()
    private val byParams = java.util.IdentityHashMap<Any, Long>()

    fun start(token: Long, paramsTag: Any): Run {
        val run = Run(token, paramsTag)
        runs[token] = run
        byParams[paramsTag] = token
        return run
    }

    fun run(token: Long): Run? = runs[token]

    fun tokenFor(paramsTag: Any): Long? = byParams[paramsTag]

    /** Registers the connection of a run that is starting; null when the run is already gone. */
    fun bind(token: Long, connection: Any): Run? {
        val run = runs[token] ?: return null
        run.connection = connection
        run.bound = true
        return run
    }

    fun bindFailed(token: Long) {
        val run = runs[token] ?: return
        run.connection = null
        run.bound = false
    }

    /** Stores a binder only for the live run; a late onServiceConnected of an old run is refused. */
    fun setBinder(token: Long, binder: Any): Boolean {
        val run = runs[token] ?: return false
        if (run.finished || !run.bound) return false
        run.binder = binder
        return true
    }

    fun clearBinding(token: Long) {
        val run = runs[token] ?: return
        run.binder = null
        run.connection = null
        run.bound = false
    }

    /** Idempotent: only the first caller receives the run and may send jobFinished. */
    fun markFinished(token: Long): Run? {
        val run = runs[token] ?: return null
        if (run.finished) return null
        run.finished = true
        return run
    }

    /** Idempotent per-run cleanup; returns the removed run once. */
    fun end(token: Long): Run? {
        val run = runs.remove(token) ?: return null
        byParams.remove(run.paramsTag)
        // Transfer the detached resources to cleanup; do not erase what it must release.
        return run
    }
}

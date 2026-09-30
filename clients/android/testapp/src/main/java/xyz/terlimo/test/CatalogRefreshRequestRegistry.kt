package xyz.terlimo.test

/** Observer used by the periodic job for its own request. Top-level so pure tests can use it. */
internal interface CatalogRefreshObserver {
    fun onCompleted(requestId: String, ok: Boolean, error: String?)
}

/**
 * §26.5 exactly-once terminal delivery for periodic requests. Registration happens once; a
 * completion takes the observer out atomically, so every rejection path (changed attempt, no
 * native child, invalid cold claim, queue rejection, failed send) delivers exactly one error
 * callback and no later success can double-report.
 */
internal class CatalogRefreshRequestRegistry {
    private val observers = java.util.concurrent.ConcurrentHashMap<String, CatalogRefreshObserver>()

    fun register(requestId: String, observer: CatalogRefreshObserver): Boolean =
        observers.putIfAbsent(requestId, observer) == null

    fun take(requestId: String): CatalogRefreshObserver? = observers.remove(requestId)

    fun deliver(requestId: String, ok: Boolean, error: String?) {
        take(requestId)?.onCompleted(requestId, ok, error)
    }
}

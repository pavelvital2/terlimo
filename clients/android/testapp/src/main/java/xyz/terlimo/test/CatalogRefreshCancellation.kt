package xyz.terlimo.test

/** Invalidate on the caller thread; only native/actor cleanup may wait in the queue. */
internal class CatalogRefreshCancellation(
    private val gate: CatalogRefreshCoordinator,
    private val observers: CatalogRefreshRequestRegistry,
    private val enqueueCleanup: (() -> Unit) -> Unit,
    private val stopCycles: (List<String>) -> Unit,
) {
    fun cancel(requestId: String) {
        val plan = gate.cancelJob(requestId)
        observers.take(requestId)
        if (plan.attemptsToStop.isNotEmpty()) {
            enqueueCleanup { stopCycles(plan.attemptsToStop) }
        }
    }
}

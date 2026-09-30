package xyz.terlimo.test

import android.content.Context

/**
 * Process-wide access to the single §26.5 coordination boundary. The coordinator itself is a
 * pure class; this object only shares one instance and forwards schedule changes to the one
 * service listener that owns the actor (failures/stops are executed there, never under the
 * ownership lock).
 */
internal object CatalogRefreshScheduleState {
    val gate = CatalogRefreshCoordinator()

    private val listeners = java.util.concurrent.CopyOnWriteArrayList<(CatalogRefreshCoordinator.ScheduleChange) -> Unit>()

    fun restore(context: Context) = gate.restore(context)

    fun mode(): CatalogRefreshMode = gate.mode()

    fun currentEpoch(): Long = gate.currentEpoch()

    /** Linear schedule change: listeners are notified after the coordinator lock is released. */
    fun setMode(next: CatalogRefreshMode): CatalogRefreshCoordinator.ScheduleChange {
        val change = gate.setMode(next)
        if (change.applied) listeners.forEach { it(change) }
        return change
    }

    fun addScheduleListener(listener: (CatalogRefreshCoordinator.ScheduleChange) -> Unit) {
        listeners += listener
    }

    fun removeScheduleListener(listener: (CatalogRefreshCoordinator.ScheduleChange) -> Unit) {
        listeners -= listener
    }

    /** Tests only: reset the shared instance between cases. */
    internal fun resetForTest() {
        listeners.clear()
        gate.resetForTest()
    }
}

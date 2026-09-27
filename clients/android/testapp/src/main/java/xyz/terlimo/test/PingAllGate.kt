package xyz.terlimo.test

/**
 * Pure bounded scheduler for the explicit manual "Пинг всех" action (S5 §07.2).
 *
 * It never starts on its own, never talks to native and never mutates selection or VPN:
 * it only sequences the current verified, probeable node IDs exactly once in snapshot
 * order and tells the caller which single node to probe next. The caller reuses the
 * existing native `probe_node` (one at a time, existing 12 s deadline). Late/foreign
 * results are ignored; cancel/refresh/phase change reset the state.
 */
internal data class PingAllState(
    val active: Boolean = false,
    val queue: List<String> = emptyList(),
    val index: Int = 0,
    val expectedId: String? = null,
    val sorted: Boolean = false,
    val generation: Long = 0,
) {
    val total: Int get() = queue.size
}

internal data class PingAllStep(val state: PingAllState, val startId: String?, val ignore: Boolean)

internal object PingAllGate {
    /** Begins a run over [nodeIds] (already filtered to probeable, snapshot order). */
    fun start(current: PingAllState, nodeIds: List<String>): PingAllStep {
        val queue = nodeIds.distinct()
        if (queue.isEmpty()) return PingAllStep(current.copy(active = false, sorted = false), null, true)
        val state = PingAllState(
            active = true,
            queue = queue,
            index = 0,
            expectedId = queue.first(),
            sorted = false,
            generation = current.generation + 1,
        )
        return PingAllStep(state, queue.first(), false)
    }

    /**
     * Consumes one native result. Only the currently expected node of an active run is
     * accepted; anything else (late, foreign, after cancel) is ignored. On the last node
     * the run finishes and [PingAllState.sorted] becomes true.
     */
    fun onResult(current: PingAllState, nodeId: String): PingAllStep {
        if (!current.active || nodeId != current.expectedId) {
            return PingAllStep(current, null, true)
        }
        val nextIndex = current.index + 1
        if (nextIndex >= current.queue.size) {
            return PingAllStep(
                current.copy(active = false, index = nextIndex, expectedId = null, sorted = true), null, false)
        }
        val nextId = current.queue[nextIndex]
        return PingAllStep(
            current.copy(index = nextIndex, expectedId = nextId), nextId, false)
    }

    /** Stops the run; grouped view is restored and late results are ignored afterwards. */
    fun cancel(current: PingAllState): PingAllState =
        current.copy(active = false, queue = emptyList(), index = 0, expectedId = null, sorted = false)

    /**
     * Cancellation must restore usable controls at once: any node still marked Running is
     * moved to Cancelled so no "running" state blocks point/all ping, and a late native
     * result for it is dropped instead of resurrecting a finished run.
     */
    fun cancelPings(pings: Map<String, NodePingState>): Map<String, NodePingState> =
        pings.mapValues { (_, state) -> if (state is NodePingState.Running) NodePingState.Cancelled else state }

    /** A late result for an already-cancelled node must never be applied as a fresh value. */
    fun isLateAfterCancel(pings: Map<String, NodePingState>, nodeId: String): Boolean =
        pings[nodeId] is NodePingState.Cancelled

    fun reset(): PingAllState = PingAllState()
}

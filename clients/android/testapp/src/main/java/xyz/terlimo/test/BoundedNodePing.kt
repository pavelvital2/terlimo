package xyz.terlimo.test

internal sealed interface NodePingState {
    data object Idle : NodePingState
    /** [probeId] attributes this run to exactly one host probe request. */
    data class Running(val token: Long, val startedAtMs: Long, val deadlineMs: Long, val probeId: String) : NodePingState
    /** [rttMs] is the only value ever shown as ping; [setupMs] is diagnostic only. */
    data class Success(val rttMs: Long, val setupMs: Long? = null) : NodePingState
    data object Timeout : NodePingState
    data object Cancelled : NodePingState
    data object Failed : NodePingState
    data object Busy : NodePingState
}

internal sealed interface PingStartResult {
    data class Started(val token: Long) : PingStartResult
    data object AlreadyRunning : PingStartResult
    data object CapacityReached : PingStartResult
    data object UnknownNode : PingStartResult
}

/**
 * Pure bounded scheduler for an Android network adapter. It creates no device,
 * VPN, transport worker or fallback selection and accepts no endpoint secrets.
 */
internal class BoundedNodePing(
    nodeIds: Collection<String>,
    private val maxConcurrent: Int = 2,
    private val timeoutMs: Long = 5_000,
) {
    private val known = nodeIds.toSet().also { require(it.size == nodeIds.size && it.none(String::isBlank)) }
    private val states = known.associateWith { NodePingState.Idle as NodePingState }.toMutableMap()
    private var nextToken = 1L

    init {
        require(maxConcurrent in 1..4)
        require(timeoutMs in 250..30_000)
    }

    @Synchronized
    fun start(nodeId: String, probeId: String, nowMs: Long): PingStartResult {
        if (nodeId !in known) return PingStartResult.UnknownNode
        require(ProbeIds.isValid(probeId)) { "PROBE_ID_INVALID" }
        if (states[nodeId] is NodePingState.Running) return PingStartResult.AlreadyRunning
        if (states.values.count { it is NodePingState.Running } >= maxConcurrent) return PingStartResult.CapacityReached
        val token = nextToken++
        states[nodeId] = NodePingState.Running(token, nowMs, nowMs + timeoutMs, probeId)
        return PingStartResult.Started(token)
    }

    @Synchronized
    fun complete(nodeId: String, token: Long, nowMs: Long): Boolean {
        val running = states[nodeId] as? NodePingState.Running ?: return false
        if (running.token != token) return false
        states[nodeId] = if (nowMs >= running.deadlineMs) NodePingState.Timeout
        else NodePingState.Success((nowMs - running.startedAtMs).coerceAtLeast(0))
        return true
    }

    @Synchronized
    fun fail(nodeId: String, token: Long): Boolean = finish(nodeId, token, NodePingState.Failed)

    @Synchronized
    fun cancel(nodeId: String, token: Long): Boolean = finish(nodeId, token, NodePingState.Cancelled)

    @Synchronized
    fun expire(nowMs: Long): Set<String> {
        val expired = states.filterValues { it is NodePingState.Running && nowMs >= it.deadlineMs }.keys
        expired.forEach { states[it] = NodePingState.Timeout }
        return expired
    }

    @Synchronized
    fun snapshot(): Map<String, NodePingState> = states.toMap()

    private fun finish(nodeId: String, token: Long, state: NodePingState): Boolean {
        val running = states[nodeId] as? NodePingState.Running ?: return false
        if (running.token != token) return false
        states[nodeId] = state
        return true
    }
}

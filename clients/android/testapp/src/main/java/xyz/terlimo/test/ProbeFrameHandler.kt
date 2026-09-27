package xyz.terlimo.test

/**
 * Active attribution fence of one host probe request. [probeId] is null only for a
 * legacy command that carried no id; such a request may only be settled by a legacy
 * result. [attempt] and [runtime] are the bridge fences: a result is accepted only
 * from the same service attempt and the same runtime generation that started it.
 */
internal data class ProbeFence(
    val nodeId: String,
    val probeId: String?,
    val attempt: String,
    val runtime: Long,
)

internal sealed interface ProbeFrameDecision {
    /** The matching frame completed the request: apply [state] exactly once. */
    data class Settle(val nodeId: String, val state: NodePingState) : ProbeFrameDecision
    /** busy with a matching id: the request stays Running, no state change. */
    data object KeepRunning : ProbeFrameDecision
    /** stale/mismatched/malformed/duplicate: no state change. */
    data object Drop : ProbeFrameDecision
}

/**
 * Pure S5 07.2 probe-result handler used by SessionService. It owns the active request
 * fence (node + probe id + attempt + runtime) and the single-completion rule: only a
 * frame matching every fence may settle the request, and only once. A missing or
 * malformed id on a result for an id-bearing request is dropped; `ok` without a
 * truthful `rtt_ms` becomes Failed (never a fake RTT); `busy` retains the matching
 * Running request; `timeout`/`failed` keep their truthful status.
 */
internal class ProbeFrameHandler {
    private var active: ProbeFence? = null

    /** Registers the request the freshly published Running state belongs to. */
    @Synchronized
    fun begin(fence: ProbeFence) {
        require(fence.probeId == null || ProbeIds.isValid(fence.probeId)) { "PROBE_ID_INVALID" }
        active = fence
    }

    /** Retires the active request when it belongs to [nodeId]; returns it if retired. */
    @Synchronized
    fun cancel(nodeId: String): ProbeFence? {
        val current = active ?: return null
        if (current.nodeId != nodeId) return null
        active = null
        return current
    }

    /** True while [nodeId] still owns the active, not yet completed request. */
    @Synchronized
    fun isActive(nodeId: String): Boolean = active?.nodeId == nodeId

    @Synchronized
    fun onFrame(
        nodeId: String,
        probeId: String?,
        status: String,
        rttMs: Long?,
        setupMs: Long?,
        attempt: String,
        runtime: Long,
    ): ProbeFrameDecision {
        val fence = active ?: return ProbeFrameDecision.Drop
        if (fence.nodeId != nodeId || fence.attempt != attempt || fence.runtime != runtime) {
            return ProbeFrameDecision.Drop
        }
        val expected = fence.probeId
        if (expected != null) {
            // An id-bearing request accepts only the exact echoed id: missing, malformed
            // or foreign ids fail closed without touching the request or its state.
            if (probeId == null || probeId != expected) return ProbeFrameDecision.Drop
        } else if (probeId != null) {
            // A legacy request is settled only by a legacy result.
            return ProbeFrameDecision.Drop
        }
        return when (status) {
            "ok" -> {
                // Only a truthful echo RTT may be shown as ping; the transport setup
                // diagnostic never substitutes for it and never fakes a success.
                val state = if (rttMs != null && rttMs in 0..12_000) {
                    NodePingState.Success(rttMs, setupMs?.takeIf { it in 0..12_000 })
                } else NodePingState.Failed
                settle()
                ProbeFrameDecision.Settle(nodeId, state)
            }
            "timeout" -> { settle(); ProbeFrameDecision.Settle(nodeId, NodePingState.Timeout) }
            "failed" -> { settle(); ProbeFrameDecision.Settle(nodeId, NodePingState.Failed) }
            "cancelled" -> { settle(); ProbeFrameDecision.Settle(nodeId, NodePingState.Cancelled) }
            "busy" -> ProbeFrameDecision.KeepRunning
            else -> ProbeFrameDecision.Drop
        }
    }

    private fun settle() {
        active = null
    }
}

package xyz.terlimo.test

import java.security.SecureRandom

/**
 * S5 07.2 host probe attribution: bounded local probe_id factory matching the native
 * bridge schema (1..64 chars of [A-Za-z0-9_-]). An id is unique per generated request:
 * a monotonically increasing run sequence (one manual point request or one ping-all
 * run) plus a per-run node sequence plus process entropy, so an already queued result
 * of an old run can never match a fresh request. Entropy is injectable for tests.
 */
internal class ProbeIds(
    private val entropy: () -> Long = { SecureRandom().nextLong() },
) {
    private var runSeq = 0L
    private var nodeSeq = 0L

    /** Starts a new run and returns its sequence. */
    @Synchronized
    fun beginRun(): Long {
        runSeq++
        nodeSeq = 0
        return runSeq
    }

    /** Issues the next bounded id of the current run. */
    @Synchronized
    fun next(): String {
        nodeSeq++
        return format(runSeq, nodeSeq, entropy())
    }

    /** One manual point request: a fresh run with exactly one id. */
    @Synchronized
    fun nextManual(): String {
        beginRun()
        return next()
    }

    companion object {
        /** Native schema: 1..64 chars of [A-Za-z0-9_-]; anything else is never sent. */
        fun isValid(id: String): Boolean {
            if (id.isEmpty() || id.length > 64) return false
            return id.all { it in 'A'..'Z' || it in 'a'..'z' || it in '0'..'9' || it == '_' || it == '-' }
        }

        /** Bounded p-<runSeq>-<nodeSeq>-<hex> id; at most 52 chars for any long value. */
        fun format(runSeq: Long, nodeSeq: Long, entropy: Long): String =
            "p-${java.lang.Long.toUnsignedString(runSeq, 16)}-" +
                "${java.lang.Long.toUnsignedString(nodeSeq, 16)}-" +
                java.lang.Long.toUnsignedString(entropy, 16)
    }
}

/** One attributed request of a sequential ping-all plan. */
internal data class ProbeRequest(val nodeId: String, val probeId: String)

/**
 * Pure sequential ordering of an all-node ping run over an ordered catalog snapshot:
 * snapshot order, one active request at a time, one unique bounded id per request. It
 * creates no transport, selection or VPN work and is safe for the ping-all UI to reuse.
 */
internal class ProbeRunPlan(
    snapshot: List<String>,
    private val ids: ProbeIds,
) {
    private val order: List<String> = snapshot.toList()
    private var index = 0
    private var outstanding: String? = null

    init {
        require(order.isNotEmpty() && order.none(String::isBlank) && order.toSet().size == order.size) {
            "PROBE_PLAN_INVALID"
        }
        ids.beginRun()
    }

    /** The node whose request is still active, or null when the next may start. */
    val activeNodeId: String? get() = outstanding

    val done: Boolean get() = index >= order.size && outstanding == null

    /** Issues the next request; overlap is refused, never queued. */
    @Synchronized
    fun next(): ProbeRequest {
        check(outstanding == null) { "PROBE_PLAN_OVERLAP" }
        check(index < order.size) { "PROBE_PLAN_DONE" }
        val nodeId = order[index++]
        outstanding = nodeId
        return ProbeRequest(nodeId, ids.next())
    }

    /** Completion or cancellation of the active request permits the next one. */
    @Synchronized
    fun advance() {
        outstanding = null
    }
}

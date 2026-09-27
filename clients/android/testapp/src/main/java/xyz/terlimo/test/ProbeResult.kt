package xyz.terlimo.test

import org.json.JSONObject

/**
 * Strict, pure parse of one `node_probe_result` bridge frame (§07.2).
 *
 * `rtt_ms` is the only measured round-trip time; `transport_setup_ms` is diagnostic setup
 * time and is kept as a non-RTT Success. `busy` (a non-current node probed while Connected
 * on the current native branch) is its own honest state and never a successful measurement.
 */
internal object ProbeResult {
    private val BASE = setOf("v", "attempt_id", "type", "node_id", "status")

    fun parse(event: JSONObject): NodePingState {
        val keys = event.keys().asSequence().toSet()
        val status = event.getString("status")
        if (status == "ok") {
            // A successful native frame may carry BOTH the diagnostic transport_setup_ms and
            // the measured rtt_ms (current-node live session and non-current direct probe).
            // Only rtt_ms is the displayed/sorted RTT; setup stays diagnostic.
            check(keys == BASE + "rtt_ms" || keys == BASE + "rtt_ms" + "transport_setup_ms") {
                "BRIDGE_MESSAGE_INVALID"
            }
            val rtt = event.getLong("rtt_ms").also { check(it in 0..12_000) }
            val setup = if (event.has("transport_setup_ms")) {
                event.getLong("transport_setup_ms").takeIf { it in 0..12_000 }
            } else null
            return NodePingState.Success(rtt, setup)
        }
        // Non-ok statuses never carry measurement fields.
        check(keys == BASE) { "BRIDGE_MESSAGE_INVALID" }
        return when (status) {
            "timeout" -> NodePingState.Timeout
            "cancelled" -> NodePingState.Cancelled
            "failed" -> NodePingState.Failed
            "busy" -> NodePingState.Busy
            else -> error("BRIDGE_MESSAGE_INVALID")
        }
    }
}

/**
 * Pure decision for the manual probe actions. It never starts on its own and never runs
 * two probes at once. Manual per-node and all-node probing are available both pre-connect
 * (`CatalogReady`) and while `Connected`.
 */
internal object ProbeGate {
    enum class Tap { START, CANCEL, IGNORE }

    fun single(
        phase: String,
        nodeKnown: Boolean,
        targetRunning: Boolean,
        pingAllActive: Boolean,
        anyRunning: Boolean,
    ): Tap = when {
        targetRunning -> Tap.CANCEL
        !nodeKnown -> Tap.IGNORE
        phase !in setOf("CatalogReady", "Connected") -> Tap.IGNORE
        pingAllActive || anyRunning -> Tap.IGNORE
        else -> Tap.START
    }

    enum class AllTap { START, IGNORE }

    fun all(phase: String, pingAllActive: Boolean, anyRunning: Boolean, probeable: Int): AllTap = when {
        pingAllActive -> AllTap.IGNORE
        anyRunning -> AllTap.IGNORE
        // Manual all-node probing works both pre-connect and while Connected: the native
        // branch probes the current node on the live session and every other node on its own
        // fenced direct echo connection, without switching or touching the active session.
        phase !in setOf("CatalogReady", "Connected") -> AllTap.IGNORE
        probeable == 0 -> AllTap.IGNORE
        else -> AllTap.START
    }
}

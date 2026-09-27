package xyz.terlimo.test

internal data class NetworkCandidate(
    val internet: Boolean,
    val notVpn: Boolean,
    val validated: Boolean,
    val isDefault: Boolean,
) {
    // Eligibility for a signed/bootstrap attempt, not proof of Internet access.
    // Android's default can be the retained VPN, and validation can fail on a
    // permitted-destinations network. Native admission and HTTPS exit remain mandatory.
    val usable: Boolean get() = internet && notVpn
}

internal data class NetworkRecoveryState(
    val nodeId: String,
    val generation: Long,
    val attempt: Int,
    val deadlineElapsed: Long,
    val inFlight: Boolean = false,
    val killSwitchHeld: Boolean = true,
    // The bounded 30 s series ended without a usable physical path. The protected TUN stays
    // held and the same node is resumed automatically once a usable network returns.
    val awaitingNetwork: Boolean = false,
)

internal data class NetworkRecoveryStart(val state: NetworkRecoveryState, val delayMs: Long)

/** What the delayed series start must do when its callback actually runs. */
internal enum class RecoveryStartOutcome { START, AWAIT, DISCARD }

/** Pure policy only: runtime wiring owns Android callbacks, VPN and process cleanup. */
internal object PhysicalNetworkRecovery {
    const val WINDOW_MS = 30_000L
    private val delays = longArrayOf(0, 1_000, 3_000)

    // A new default/candidate or a validation probe failure does not invalidate
    // sockets already bound to a physical network. This matters under VPN and
    // on networks that permit only selected Internet destinations.
    fun boundPathUnavailable(candidate: NetworkCandidate): Boolean = !candidate.internet || !candidate.notVpn

    private val recoveryPhases = setOf("Connected", "SwitchingServer", "SleepPaused")

    // Recovery may start from a healthy tunnel, a server switch, or battery sleep
    // resume; an already-running recovery may continue from any phase.
    fun recoveryAllowed(phase: String, hasRecovery: Boolean): Boolean =
        hasRecovery || phase in recoveryPhases

    fun begin(nodeId: String, generation: Long, nowElapsed: Long): NetworkRecoveryState {
        require(nodeId.isNotBlank() && generation >= 0 && nowElapsed >= 0) { "NETWORK_RECOVERY_INVALID" }
        return NetworkRecoveryState(nodeId, generation + 1, 0, nowElapsed + WINDOW_MS)
    }

    fun ready(state: NetworkRecoveryState, generation: Long, candidate: NetworkCandidate,
        nowElapsed: Long): NetworkRecoveryStart? {
        if (generation != state.generation || state.inFlight || nowElapsed > state.deadlineElapsed || !candidate.usable) return null
        return retry(state, nowElapsed)
    }

    fun admit(state: NetworkRecoveryState): NetworkRecoveryState {
        require(!state.inFlight) { "NETWORK_RECOVERY_ACTIVE" }
        return state.copy(inFlight = true)
    }

    fun retry(state: NetworkRecoveryState, nowElapsed: Long): NetworkRecoveryStart? {
        if (state.inFlight || nowElapsed > state.deadlineElapsed || state.attempt !in delays.indices) return null
        val delay = delays[state.attempt]
        if (nowElapsed + delay > state.deadlineElapsed) return null
        return NetworkRecoveryStart(state.copy(attempt = state.attempt + 1), delay)
    }

    fun established(state: NetworkRecoveryState, generation: Long, nodeId: String): Boolean =
        generation == state.generation && nodeId == state.nodeId

    /** The bounded series window is over. */
    fun expired(state: NetworkRecoveryState, nowElapsed: Long): Boolean = nowElapsed > state.deadlineElapsed

    /**
     * A delayed series start ran after its candidate disappeared (or the window passed first).
     * Cancelling must never leave a state without retry or wait: start when still possible,
     * enter the waiting state otherwise, and discard stale/stopped/already-waiting callbacks.
     */
    fun scheduledStartOutcome(state: NetworkRecoveryState?, generation: Long, stopping: Boolean,
        activeAttempt: Boolean, usablePath: Boolean): RecoveryStartOutcome = when {
        state == null || stopping || activeAttempt || state.awaitingNetwork ||
            state.generation != generation -> RecoveryStartOutcome.DISCARD
        usablePath -> RecoveryStartOutcome.START
        else -> RecoveryStartOutcome.AWAIT
    }

    /**
     * The bounded series is over with no usable physical path: keep the selected node and the
     * protected TUN, and wait for a network callback. No attempt, timer or busy loop runs.
     */
    fun await(state: NetworkRecoveryState): NetworkRecoveryState =
        state.copy(inFlight = false, awaitingNetwork = true)

    /**
     * A usable physical network returned while awaiting, or a cancelled start left an expired
     * series: start a new bounded series for the SAME node with a fresh generation. A stopped
     * owner, a stale generation callback, an in-flight attempt, an unusable network or an
     * expired/revoked right must not restart anything.
     */
    fun resume(state: NetworkRecoveryState, generation: Long, candidate: NetworkCandidate,
        nowElapsed: Long, leaseValid: Boolean, stopping: Boolean): NetworkRecoveryStart? {
        if (stopping || state.inFlight || generation != state.generation || !candidate.usable ||
            !leaseValid || !(state.awaitingNetwork || nowElapsed > state.deadlineElapsed)) return null
        val restarted = state.copy(generation = state.generation + 1, attempt = 0,
            deadlineElapsed = nowElapsed + WINDOW_MS, inFlight = false, killSwitchHeld = true,
            awaitingNetwork = false)
        return NetworkRecoveryStart(restarted, 0L)
    }

    fun shouldSelectNode(state: NetworkRecoveryState, attemptId: String, selectedAttemptId: String?,
        phase: String, selectedNodeId: String, catalogNodeIds: Set<String>): Boolean =
        attemptId != selectedAttemptId && phase == "CatalogReady" && selectedNodeId == state.nodeId &&
            state.nodeId in catalogNodeIds
}

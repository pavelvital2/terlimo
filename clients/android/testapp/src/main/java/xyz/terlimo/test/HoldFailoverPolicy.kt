package xyz.terlimo.test

/** The previous native child is still being stopped; the armed target waits for its completion. */
internal data class HoldFailoverWait(val target: String, val stopGeneration: Long)

/**
 * The real held-gateway failover state owned by SessionService. It is a pure value so the whole
 * lifecycle (tap during child reap, completion, stale completion, cancel) is directly testable.
 */
internal data class HoldFailoverState(
    val armed: String? = null,
    val wait: HoldFailoverWait? = null,
    val attempt: String? = null,
    val nodeId: String? = null,
) {
    val pending: Boolean get() = armed != null || wait != null
}

/** Result of a held-failover transition. */
internal sealed class HoldFailoverStart {
    /** Nothing applies (no pending choice, or a stale completion): keep the current state. */
    object Ignored : HoldFailoverStart()
    /** Cancel the pending choice without starting anything (stopping/expired/vanished/failed stop). */
    object Rejected : HoldFailoverStart()
    /** The previous child is confirmed stopped and all checks pass: this is the only start point. */
    data class Start(val target: String) : HoldFailoverStart()
    /** The previous child is still stopping: keep the armed target and wait for completion. */
    data class Wait(val target: String, val stopGeneration: Long) : HoldFailoverStart()
}

/**
 * Behaviour of an explicit gateway choice while a protected hold is active (KillSwitch).
 *
 * UX §5: when the server is unavailable or a switch failed, protected traffic stays blocked and
 * another gateway remains selectable. The tap may only start a fresh attempt for the explicitly
 * chosen node, and only after the previous native child stop is confirmed for the current stop
 * generation; it never revives cached access, never changes the automatic selection and never
 * releases the retained protection. A failed stop or a stale completion never starts a new child.
 */
internal object HoldFailoverPolicy {
    /**
     * A held (KillSwitch) tap may start a fresh attempt only when the protection is actually
     * applied, the target still exists in the verified catalog, no other choice is pending and
     * the lease is still valid (expired/revoked access is never revived from cache).
     */
    fun canStart(phase: String, protectionApplied: Boolean, targetInCatalog: Boolean,
        pending: Boolean, leaseValid: Boolean): Boolean =
        phase == "KillSwitch" && protectionApplied && targetInCatalog && !pending && leaseValid

    /**
     * Fresh verified catalog after an armed tap: only the explicitly chosen target may be
     * selected. A missing target keeps the hold and never falls back to another node.
     */
    fun catalogTarget(armedTarget: String?, catalogIds: Set<String>): String? =
        armedTarget?.takeIf { it in catalogIds }

    /**
     * Outcome of a failed attempt started from the hold: a retained protection is held
     * (KillSwitch with the choice still available), never released into a direct exit.
     */
    fun failureOutcome(protectionApplied: Boolean): TerminalFailureOutcome =
        if (protectionApplied) TerminalFailureOutcome.HOLD else TerminalFailureOutcome.STOP

    /**
     * A tap during the hold. `nativeStopped` is the confirmed completion of the previous child
     * for `stopGeneration`. While it is not confirmed the target is only armed and the start
     * waits for [childStopped]; a second tap is rejected while any choice is pending.
     */
    fun tap(state: HoldFailoverState, target: String, phase: String, protectionApplied: Boolean,
        targetInCatalog: Boolean, leaseValid: Boolean, nativeStopped: Boolean,
        stopGeneration: Long): Pair<HoldFailoverState, HoldFailoverStart> {
        if (!canStart(phase, protectionApplied, targetInCatalog, state.pending, leaseValid)) {
            return state to HoldFailoverStart.Rejected
        }
        return if (nativeStopped) {
            state.copy(armed = target, wait = null) to HoldFailoverStart.Start(target)
        } else {
            state.copy(wait = HoldFailoverWait(target, stopGeneration)) to
                HoldFailoverStart.Wait(target, stopGeneration)
        }
    }

    /**
     * Completion of the previous child stop (or its failure). Only the exact pending generation
     * may start; a failed/unconfirmed stop or a changed hold condition cancels the choice without
     * starting anything. Stale completions leave the current state untouched.
     */
    fun childStopped(state: HoldFailoverState, stopGeneration: Long, stopConfirmed: Boolean,
        stopping: Boolean, phase: String, protectionApplied: Boolean, targetInCatalog: Boolean,
        leaseValid: Boolean): Pair<HoldFailoverState, HoldFailoverStart> {
        val wait = state.wait ?: return state to HoldFailoverStart.Ignored
        if (wait.stopGeneration != stopGeneration) return state to HoldFailoverStart.Ignored
        if (!stopConfirmed || stopping || !canStart(phase, protectionApplied, targetInCatalog,
                pending = false, leaseValid)) {
            return state.copy(armed = null, wait = null) to HoldFailoverStart.Rejected
        }
        return state.copy(armed = wait.target, wait = null) to HoldFailoverStart.Start(wait.target)
    }

    /** The fresh verified catalog accepted the armed target for this attempt. */
    fun catalogAccepted(state: HoldFailoverState, attempt: String, target: String): HoldFailoverState =
        state.copy(armed = null, attempt = attempt, nodeId = target)

    /**
     * A stop completion may update the global stop readiness only when it belongs to the current
     * stop generation and the owner is not stopping. A stale completion (new hold, recovery or
     * Disconnect invalidated it) must change nothing, even if no pending choice exists any more.
     */
    fun completionApplies(stopGeneration: Long, currentStopGeneration: Long, stopping: Boolean): Boolean =
        !stopping && stopGeneration == currentStopGeneration

    /** Any lifecycle change (Disconnect, expiry, recovery, new hold, apply) cancels the choice. */
    fun cleared(): HoldFailoverState = HoldFailoverState()
}

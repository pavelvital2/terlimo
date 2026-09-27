package xyz.terlimo.test

/**
 * Terminal failure routing for unexpected native/bridge/server errors.
 *
 * Only an explicit host stop (`stopAttempt(null)` from Disconnect/"stopped") or an OS VPN
 * revoke may release the application-level protection. An unexpected terminal error while a
 * tunnel is actually applied must hold the protected TUN (KillSwitch) instead of tearing it
 * down, so protected applications stay blocked and Disconnect remains available.
 *
 * Before the first applied TUN there is nothing to hold: the attempt stops and never
 * fabricates a VPN. An already-running bounded network recovery keeps retrying while a lease
 * is valid; expiry/revoke never creates a new right.
 */
internal enum class TerminalFailureOutcome { STOP, HOLD, RETRY_RECOVERY }

/** Ownership-fenced routing for a terminal failure that may carry an attempt id. */
internal enum class TerminalDecision { IGNORE_STALE, STOP, HOLD, RETRY_RECOVERY }

internal object TerminalFailurePolicy {
    /** Same set as the bounded physical-network recovery retries. */
    val RETRYABLE_CODES = setOf("PHYSICAL_NETWORK_UNAVAILABLE", "PHYSICAL_NETWORK_LOST",
        "TRANSPORT_FAILED", "HOST_ERROR", "CATALOG_TIMEOUT")

    fun outcome(code: String, recoveryActive: Boolean, protectionApplied: Boolean): TerminalFailureOutcome = when {
        recoveryActive -> when {
            code in RETRYABLE_CODES -> TerminalFailureOutcome.RETRY_RECOVERY
            protectionApplied -> TerminalFailureOutcome.HOLD
            else -> TerminalFailureOutcome.STOP
        }
        protectionApplied -> TerminalFailureOutcome.HOLD
        else -> TerminalFailureOutcome.STOP
    }

    /**
     * Full decision including attempt ownership.
     *
     * A failure tagged with another attempt id is stale and must not touch the current
     * attempt. A failure without a live attempt owner never bypasses the protection policy:
     * an applied TUN is held, and no retry/attempt is fabricated from it.
     */
    fun decide(failureAttempt: String?, activeAttempt: String?, code: String,
        recoveryActive: Boolean, protectionApplied: Boolean): TerminalDecision = when {
        failureAttempt != null && failureAttempt != activeAttempt -> TerminalDecision.IGNORE_STALE
        activeAttempt == null -> when (outcome(code, recoveryActive, protectionApplied)) {
            TerminalFailureOutcome.STOP -> TerminalDecision.STOP
            // There is no live attempt to retry; hold an actually applied protection.
            TerminalFailureOutcome.HOLD, TerminalFailureOutcome.RETRY_RECOVERY ->
                if (protectionApplied) TerminalDecision.HOLD else TerminalDecision.STOP
        }
        else -> when (outcome(code, recoveryActive, protectionApplied)) {
            TerminalFailureOutcome.STOP -> TerminalDecision.STOP
            TerminalFailureOutcome.HOLD -> TerminalDecision.HOLD
            TerminalFailureOutcome.RETRY_RECOVERY -> TerminalDecision.RETRY_RECOVERY
        }
    }
}

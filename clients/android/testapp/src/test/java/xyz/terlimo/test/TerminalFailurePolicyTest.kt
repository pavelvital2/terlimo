package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * Behavioural tests for unexpected terminal failures: only an explicit host stop or an OS
 * revoke may release the application protection. Before the first applied TUN nothing holds;
 * after it, terminal server/transport/revoke errors must hold, never tear down or retry a
 * revoked right.
 */
class TerminalFailurePolicyTest {
    private fun terminal(code: String, recovery: Boolean, protection: Boolean) =
        TerminalFailurePolicy.outcome(code, recoveryActive = recovery, protectionApplied = protection)

    @Test fun firstAttemptFailureBeforeAnyTunnelStopsAndNeverFabricatesHold() {
        assertEquals(TerminalFailureOutcome.STOP, terminal("GRANT_REVOKED", false, false))
        assertEquals(TerminalFailureOutcome.STOP, terminal("TRANSPORT_FAILED", false, false))
        assertEquals(TerminalFailureOutcome.STOP, terminal("NATIVE_EXIT", false, false))
        for (code in TerminalFailurePolicy.RETRYABLE_CODES + setOf("GRANT_REVOKED", "DEVICE_REVOKED")) {
            assertEquals(code, TerminalFailureOutcome.STOP, terminal(code, false, false))
        }
    }

    @Test fun appliedProtectionHoldsEveryUnexpectedTerminal() {
        for (code in TerminalFailurePolicy.RETRYABLE_CODES +
            setOf("NATIVE_EXIT", "BRIDGE_INVALID", "GRANT_REVOKED", "DEVICE_REVOKED", "LEASE_EXPIRED",
                "PROOF_INVALID", "AUTH_REQUIRED")) {
            assertEquals(code, TerminalFailureOutcome.HOLD, terminal(code, false, true))
        }
    }

    @Test fun retryableRecoveryTerminalKeepsTheBoundedRecoverySeries() {
        for (code in TerminalFailurePolicy.RETRYABLE_CODES) {
            assertEquals(code, TerminalFailureOutcome.RETRY_RECOVERY, terminal(code, true, true))
        }
    }

    @Test fun revokedOrExpiredRightNeverGetsAnAutomaticRetry() {
        for (code in listOf("GRANT_REVOKED", "DEVICE_REVOKED", "LEASE_EXPIRED")) {
            assertEquals(code, TerminalFailureOutcome.HOLD, terminal(code, true, true))
        }
    }

    @Test fun recoveryWithoutAppliedProtectionNeverFabricatesHold() {
        // A retryable recovery code still means "retry", but with no live attempt or applied
        // TUN the full decision must stop instead of claiming a blocked state it cannot enforce.
        assertEquals(TerminalFailureOutcome.RETRY_RECOVERY, terminal("TRANSPORT_FAILED", true, false))
        assertEquals(TerminalFailureOutcome.STOP, terminal("GRANT_REVOKED", true, false))
        assertEquals(TerminalDecision.STOP, decision(null, null, "TRANSPORT_FAILED", true, false))
        assertEquals(TerminalDecision.STOP, decision(null, null, "GRANT_REVOKED", true, false))
    }

    private fun decision(failureAttempt: String?, activeAttempt: String?, code: String,
        recovery: Boolean, protection: Boolean) = TerminalFailurePolicy.decide(failureAttempt, activeAttempt,
        code, recoveryActive = recovery, protectionApplied = protection)

    @Test fun attemptLessOverflowNeverReleasesAnAppliedProtection() {
        // D1: a host-side overflow has no attempt id; it must not bypass the policy and
        // tear down an actually applied TUN (e.g. actor overflow while a hold is active).
        assertEquals(TerminalDecision.HOLD, decision(null, null, "BRIDGE_CONTROL_OVERFLOW", false, true))
        assertEquals(TerminalDecision.HOLD, decision(null, null, "BRIDGE_CONTROL_OVERFLOW", true, true))
        // Before any applied TUN it is still an ordinary stop.
        assertEquals(TerminalDecision.STOP, decision(null, null, "BRIDGE_CONTROL_OVERFLOW", false, false))
        assertEquals(TerminalDecision.STOP, decision(null, null, "BRIDGE_CONTROL_OVERFLOW", true, false))
    }

    @Test fun staleFailureNeverTouchesTheNewAttempt() {
        assertEquals(TerminalDecision.IGNORE_STALE,
            decision("old-attempt", "new-attempt", "NATIVE_EXIT", false, true))
        assertEquals(TerminalDecision.IGNORE_STALE,
            decision("old-attempt", "new-attempt", "TRANSPORT_FAILED", true, true))
        assertEquals(TerminalDecision.IGNORE_STALE,
            decision("old-attempt", null, "BRIDGE_INVALID", false, true))
    }

    @Test fun liveAttemptFailureFollowsTheFullPolicy() {
        assertEquals(TerminalDecision.STOP, decision("a1", "a1", "GRANT_REVOKED", false, false))
        assertEquals(TerminalDecision.HOLD, decision("a1", "a1", "GRANT_REVOKED", false, true))
        assertEquals(TerminalDecision.RETRY_RECOVERY, decision("a1", "a1", "TRANSPORT_FAILED", true, true))
    }

    @Test fun resumedAttemptApplyFailureKeepsTheRetainedTunnel() {
        // Runtime long-loss: the resumed recovery attempt failed (native preparation budget
        // expired with no usable TUN). With the retained config still applied the decision must
        // hold the protection, never release it into a direct exit.
        for (code in listOf("VPN_SETUP_TIMEOUT", "VPN_READINESS_FAILED", "VPN_NETWORK_UNAVAILABLE")) {
            assertEquals(code, TerminalFailureOutcome.HOLD, terminal(code, true, true))
            assertEquals(code, TerminalDecision.HOLD, decision("resumed", "resumed", code, true, true))
        }
        // Without an actually applied protection the same failure stays a normal stop.
        assertEquals(TerminalDecision.STOP, decision("resumed", "resumed", "VPN_SETUP_TIMEOUT", true, false))
    }
}

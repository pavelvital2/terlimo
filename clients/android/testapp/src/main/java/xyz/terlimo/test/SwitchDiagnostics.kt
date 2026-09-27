package xyz.terlimo.test

import android.util.Log

/**
 * Bounded, secret-free stage diagnostics for an explicit manual gateway switch only.
 *
 * One fixed line per boundary lets a single bounded tap distinguish UI request, service
 * admission, control queue, host write, native bridge/runner and the received result.
 * Only fixed stage/reason tokens (or the fixed `unknown` token) can ever be written; raw
 * node/switch/revision/session/link/key values, payloads and exception text never pass.
 */
internal object SwitchDiagnostics {
    const val TAG = "WDTT/Switch"
    private val STAGES = setOf(
        "ui_request", "ui_reject",
        "service_received", "service_reject",
        "queued", "action_entered", "action_reject",
        "host_send", "native", "result_received",
    )
    private val REASONS = setOf(
        // UI / service admission
        "phase", "stopping", "retiring", "unknown_target", "local_pending", "service_pending",
        "same_target", "not_connected", "pending", "no_revision", "recheck",
        // outcomes / host write / result
        "ok", "failed", "stale", "no_native", "gate", "write", "closing",
        // native bridge / runner
        "bridge_accepted", "bridge_full", "runner_consumed", "runner_result_ok", "runner_result_failed",
        "unknown",
    )

    /** Pure fixed-format line; unknown stages/reasons collapse to the fixed `unknown` token. */
    fun line(stage: String, reason: String? = null): String {
        val safeStage = if (stage in STAGES) stage else "unknown"
        val safeReason = reason?.let { if (it in REASONS) it else "unknown" } ?: "-"
        return "switch stage=$safeStage reason=$safeReason"
    }

    fun log(stage: String, reason: String? = null) = Log.w(TAG, line(stage, reason))

    /** Fixed reason for an admission result; null means allowed. */
    fun admissionReason(value: SwitchAdmission): String? = when (value) {
        SwitchAdmission.ALLOWED -> null
        SwitchAdmission.NOT_CONNECTED -> "not_connected"
        SwitchAdmission.PENDING -> "pending"
        SwitchAdmission.NO_REVISION -> "no_revision"
        SwitchAdmission.SAME_TARGET -> "same_target"
        SwitchAdmission.UNKNOWN_TARGET -> "unknown_target"
    }
}

package xyz.terlimo.test

import org.json.JSONObject

/** Fixed, payload-free diagnostic schema. Invalid telemetry must not affect the attempt. */
internal object BootstrapDiagnostics {
    private val enums = mapOf(
        "operation" to setOf("UNKNOWN", "CHALLENGE", "CATALOG", "STATUS", "REFRESH", "REGISTER"),
        "io_stage" to setOf("DIAL", "HANDSHAKE", "WRITE", "READ", "COMPLETE"),
        "first_close" to setOf("NONE", "UNKNOWN", "RELAY_READ", "RELAY_WRITE", "PIPE_READ", "PIPE_WRITE", "CALLER_CANCEL", "CLEANUP"),
        "error_class" to setOf("NONE", "EOF", "CANCELED", "DEADLINE", "OTHER"))
    private val booleans = setOf("handshake_pin_ok", "caller_canceled", "caller_deadline",
        "transport_canceled", "transport_deadline", "failed")
    private val fields = enums.keys + booleans + "elapsed_ms"

    fun parse(event: JSONObject, attempt: String): Map<String, Any>? {
        if (event.keys().asSequence().toSet() != fields + setOf("v", "type", "attempt_id")) return null
        if (integer(event.opt("v")) != 1L || event.opt("type") != "bootstrap_diagnostic" ||
            event.opt("attempt_id") != attempt) return null
        for ((key, allowed) in enums) if (event.opt(key) !in allowed) return null
        for (key in booleans) if (event.opt(key) !is Boolean) return null
        val elapsed = integer(event.opt("elapsed_ms"))?.takeIf { it >= 0 } ?: return null
        return fields.associateWith { if (it == "elapsed_ms") elapsed else event.get(it) }
    }

    // JSON integer types only: no string coercion, fractions, overflow or NaN.
    private fun integer(value: Any?): Long? = when (value) {
        is Int -> value.toLong()
        is Long -> value
        else -> null
    }

    fun retainFirstFailure(previous: Map<String, Any>, next: Map<String, Any>): Map<String, Any> =
        if (previous["failed"] == true) previous else next
}

package xyz.terlimo.test

import org.json.JSONObject

internal const val MAX_WAKE_INTEGER = 9_007_199_254_740_991L
internal fun wakeInteger(value: Any?): Long? = when (value) {
    is Int -> value.toLong().takeIf { it > 0 }
    is Long -> value.takeIf { it in 1..MAX_WAKE_INTEGER }
    else -> null
}

/** Informational worker readiness; never substitutes for DNS/HTTPS admission. */
internal data class WakeRecoveryStatus(val runtimeEpoch: Long, val lifecycleRevision: Long,
    val generation: Long, val ready: Int, val total: Int) {
    init {
        require(runtimeEpoch in 1..MAX_WAKE_INTEGER && lifecycleRevision in 1..MAX_WAKE_INTEGER &&
            generation in 1..MAX_WAKE_INTEGER && total in 0..36 && ready in 0..total)
    }
    val text: String get() = if (ready == 0) "Проверяем каналы: 0 из $total" else "Готовые каналы: $ready из $total"
}

internal object WakeRecoveryProjection {
    private val fields = setOf("v", "attempt_id", "type", "runtime_epoch", "lifecycle_revision", "generation", "ready", "total")
    fun parse(event: JSONObject): WakeRecoveryStatus? {
        if (event.keys().asSequence().toSet() != fields || event.opt("type") != "wake_status" || wakeInteger(event.opt("v")) != 1L) return null
        val epoch = wakeInteger(event.opt("runtime_epoch")) ?: return null
        val lifecycle = wakeInteger(event.opt("lifecycle_revision")) ?: return null
        val generation = wakeInteger(event.opt("generation")) ?: return null
        fun count(name: String): Int? = when (val value = event.opt(name)) {
            is Int -> value.takeIf { it in 0..36 }
            is Long -> value.takeIf { it in 0..36 }?.toInt()
            else -> null
        }
        val ready = count("ready") ?: return null
        val total = count("total") ?: return null
        if (ready > total) return null
        return WakeRecoveryStatus(epoch, lifecycle, generation, ready, total)
    }
    fun accept(current: WakeRecoveryStatus?, incoming: WakeRecoveryStatus,
        activeAttempt: String?, eventAttempt: String, activeRuntimeEpoch: Long, lifecycleRevision: Long,
        interactive: Boolean, stopping: Boolean): WakeRecoveryStatus? {
        if (stopping || !interactive || activeAttempt == null || activeAttempt != eventAttempt ||
            incoming.runtimeEpoch != activeRuntimeEpoch || incoming.lifecycleRevision != lifecycleRevision) return current
        if (current != null && current.runtimeEpoch == incoming.runtimeEpoch &&
            current.lifecycleRevision == incoming.lifecycleRevision && incoming.generation < current.generation) return current
        return incoming
    }
}

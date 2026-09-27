package xyz.terlimo.test

import org.json.JSONObject

internal data class ChannelsStatus(val runtimeEpoch: Long, val lifecycleRevision: Long,
    val generation: Long, val active: Int, val target: Int) {
    init {
        require(runtimeEpoch in 1..MAX_WAKE_INTEGER && lifecycleRevision in 0..MAX_WAKE_INTEGER &&
            generation in 0..MAX_WAKE_INTEGER && target in 0..36 && active in 0..target)
    }
    val text: String? get() = if (target == 0) null else "Каналы: $active из $target"
}

internal object ChannelsDisplay {
    fun line(connected: Boolean, channels: ChannelsStatus?, wakeRecovery: WakeRecoveryStatus?): String? = when {
        wakeRecovery != null -> wakeRecoveryLine(wakeRecovery, channels?.target)
        connected -> channels?.text
        else -> null
    }

    fun wakeRecoveryLine(wake: WakeRecoveryStatus, target: Int?): String {
        val denominator = target?.takeIf { it > 0 }
        return when {
            wake.ready == 0 && denominator != null -> "Проверяем каналы: 0 из $denominator"
            wake.ready == 0 -> "Проверяем каналы: 0"
            denominator != null -> "Готовые каналы: ${wake.ready} из $denominator"
            else -> "Готовые каналы: ${wake.ready}"
        }
    }
}

internal object ChannelsStatusProjection {
    private val fields = setOf("v", "attempt_id", "type", "runtime_epoch", "lifecycle_revision", "generation", "active", "target")
    fun parse(event: JSONObject): ChannelsStatus? {
        if (event.keys().asSequence().toSet() != fields || event.opt("type") != "channels_status" || wakeInteger(event.opt("v")) != 1L) return null
        val epoch = wakeInteger(event.opt("runtime_epoch")) ?: return null
        val lifecycle = when (val value = event.opt("lifecycle_revision")) {
            is Int -> value.toLong().takeIf { it in 0..MAX_WAKE_INTEGER }
            is Long -> value.takeIf { it in 0..MAX_WAKE_INTEGER }
            else -> null
        } ?: return null
        val generation = when (val value = event.opt("generation")) {
            is Int -> value.toLong().takeIf { it >= 0 }
            is Long -> value.takeIf { it in 0..MAX_WAKE_INTEGER }
            else -> null
        } ?: return null
        fun count(name: String): Int? = when (val value = event.opt(name)) {
            is Int -> value.takeIf { it in 0..36 }
            is Long -> value.takeIf { it in 0..36 }?.toInt()
            else -> null
        }
        val active = count("active") ?: return null
        val target = count("target") ?: return null
        if (active > target) return null
        return ChannelsStatus(epoch, lifecycle, generation, active, target)
    }

    fun accept(current: ChannelsStatus?, incoming: ChannelsStatus, activeAttempt: String?, eventAttempt: String,
        activeRuntimeEpoch: Long, lifecycleRevision: Long, interactive: Boolean, stopping: Boolean): ChannelsStatus? {
        if (stopping || !interactive || activeAttempt == null || activeAttempt != eventAttempt ||
            incoming.runtimeEpoch != activeRuntimeEpoch || incoming.lifecycleRevision != lifecycleRevision) return current
        if (current != null && current.runtimeEpoch == incoming.runtimeEpoch &&
            current.lifecycleRevision == incoming.lifecycleRevision && incoming.generation < current.generation) return current
        return incoming
    }
}

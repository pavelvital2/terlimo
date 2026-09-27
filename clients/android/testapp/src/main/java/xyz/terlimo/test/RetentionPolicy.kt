package xyz.terlimo.test

internal enum class RetentionMode(val storedValue: String) {
    BALANCED("balanced"), HOLD_CONNECTION("hold_connection"), SAVE_BATTERY("save_battery");
    companion object {
        fun fromStored(value: String?): RetentionMode = entries.firstOrNull { it.storedValue == value } ?: BALANCED
    }
}

internal data class RetentionLocks(val cpu: Boolean, val wifi: Boolean)

/** v18 TunnelService shouldKeepHoldModeCpuLock / shouldHoldBackgroundWifiRadio.
 * Access lifetime intentionally is not an input to connection retention. */
internal object RetentionPolicy {
    fun locks(mode: RetentionMode, running: Boolean, paused: Boolean, stopping: Boolean,
        networkAvailable: Boolean, interactive: Boolean, wifi: Boolean): RetentionLocks {
        val cpu = mode == RetentionMode.HOLD_CONNECTION && running && !paused && !stopping && networkAvailable
        return RetentionLocks(cpu, cpu && !interactive && wifi)
    }
}

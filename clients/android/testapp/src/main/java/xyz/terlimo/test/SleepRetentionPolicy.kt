package xyz.terlimo.test

internal data class SleepRetentionCycle(val epoch: Long, val pauseAt: Long, val resumeAt: Long?)

/** v18 delayed_pause / timed_pause and shortSleepResumeGuardDurationMs. */
internal object SleepRetentionPolicy {
    const val DEFAULT_PAUSE_MINUTES = 5
    const val MAX_MINUTES = 24 * 60

    fun begin(epoch: Long, now: Long, pauseMinutes: Int, resumeMinutes: Int?): SleepRetentionCycle? {
        require(epoch > 0 && now >= 0)
        if (resumeMinutes == 0) return null // v18: a zero resume timer keeps this sleep cycle running.
        val pause = if (resumeMinutes == null) pauseMinutes.coerceIn(0, MAX_MINUTES) else 0
        val resume = resumeMinutes?.coerceIn(0, MAX_MINUTES)?.toLong()?.times(60_000)
        return SleepRetentionCycle(epoch, now + pause * 60_000L, resume?.let { now + it })
    }

    fun pauseDue(cycle: SleepRetentionCycle, epoch: Long, now: Long, interactive: Boolean,
        stopped: Boolean, connected: Boolean): Boolean =
        cycle.epoch == epoch && now >= cycle.pauseAt && !interactive && !stopped && connected

    fun resumeDue(cycle: SleepRetentionCycle, epoch: Long, now: Long, interactive: Boolean,
        stopped: Boolean, paused: Boolean): Boolean = cycle.epoch == epoch && !stopped && paused &&
        (interactive || cycle.resumeAt?.let { now >= it } == true)

    fun shortGuardMillis(deadline: Long, now: Long): Long? =
        (deadline - now).takeIf { it in 1..120_000 }?.plus(10_000)
}

package xyz.terlimo.test

/**
 * §26.5 catalog auto-update schedule. Pure policy: the app only ever holds one periodic
 * JobScheduler entry, and Off is the default. Nothing here touches VPN, access/lease sync,
 * node selection or any product timeout.
 */
internal enum class CatalogRefreshMode(val stored: String) {
    OFF("off"),
    TWICE_DAILY("twice_daily"),
    DAILY("daily"),
    WEEKLY("weekly");

    companion object {
        fun fromStored(value: String?): CatalogRefreshMode =
            entries.firstOrNull { it.stored == value } ?: OFF
    }
}

internal object CatalogRefreshPolicy {
    const val JOB_ID = 7326

    /** Approved UI labels for the four modes (Off is the default). */
    fun label(mode: CatalogRefreshMode): String = when (mode) {
        CatalogRefreshMode.OFF -> "Выключено"
        CatalogRefreshMode.TWICE_DAILY -> "2 раза в день"
        CatalogRefreshMode.DAILY -> "Ежедневно"
        CatalogRefreshMode.WEEKLY -> "Раз в неделю"
    }

    /**
     * Honest status line: a refused schedule is reported, never shown as an active period;
     * periods are explicitly approximate (JobScheduler owns timing).
     */
    fun statusText(mode: CatalogRefreshMode, refused: Boolean): String = when {
        mode == CatalogRefreshMode.OFF -> "Обновление каталога выключено."
        refused -> "Не удалось запланировать обновление каталога. Расписание не активно."
        else -> "Обновление каталога: ${label(mode)} (примерно)."
    }

    /** JobScheduler.RESULT_SUCCESS is 1; kept here so the mapping stays pure/testable. */
    const val SCHEDULE_SUCCESS = 1

    /**
     * True when a mode is requested but the platform did not actually accept the periodic
     * entry (or the scheduler threw). Off is never "refused": it is simply off. An UNCHANGED
     * decision means the existing entry already matches, so it is not a refusal.
     */
    fun refused(mode: CatalogRefreshMode, decision: Decision, scheduleResult: Int?, threw: Boolean = false): Boolean {
        if (mode == CatalogRefreshMode.OFF) return false
        if (threw) return true
        return decision == Decision.SCHEDULE && scheduleResult != SCHEDULE_SUCCESS
    }

    fun intervalMillis(mode: CatalogRefreshMode): Long? = when (mode) {
        CatalogRefreshMode.OFF -> null
        CatalogRefreshMode.TWICE_DAILY -> 12L * 60 * 60 * 1000
        CatalogRefreshMode.DAILY -> 24L * 60 * 60 * 1000
        CatalogRefreshMode.WEEKLY -> 7L * 24 * 60 * 60 * 1000
    }

    /**
     * A single pending entry is kept: a matching interval keeps the existing period untouched
     * (an Activity recreation must never restart the next run), a changed mode replaces it,
     * Off cancels it. There is deliberately no catch-up: JobScheduler owns the next window.
     */
    enum class Decision { UNCHANGED, SCHEDULE, CANCEL }

    fun reconcile(pendingInterval: Long?, desired: CatalogRefreshMode): Decision {
        val desiredInterval = intervalMillis(desired)
        if (desiredInterval == null) return Decision.CANCEL
        return if (pendingInterval == desiredInterval) Decision.UNCHANGED else Decision.SCHEDULE
    }
}

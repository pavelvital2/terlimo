package xyz.terlimo.test

/**
 * Display-only device-usage line for the Subscription screen (S5 §07.1).
 *
 * It is derived exclusively from the SAME accepted `/me` entitlement snapshot
 * (`slots_used` + `effective_device_limit`); it never mixes the catalog's own slot
 * counts with an entitlement limit. Both numbers are always reported exactly as the
 * server sent them — an inconsistent but syntactically valid over-limit state is shown
 * as-is (e.g. `5 из 3`) and is never clamped into a different used count. The line is
 * shown only while the snapshot is the current successful `/me` of the live attempt
 * ([AccountAccessSnapshot.current]); a retained last-good snapshot after a terminal
 * stop/hold, an absent `/me`, a non-active entitlement, or an unusable server limit all
 * yield no line — a stale or unknown count is never presented as current account state.
 * Display only: it never gates, writes, fetches or derives eligibility.
 */
internal object SubscriptionDeviceText {
    fun line(snapshot: AccountAccessSnapshot?): String? {
        val projection = snapshot?.takeIf { it.current }?.projection ?: return null
        val entitlement = projection.entitlement
        if (entitlement.status != "active") return null
        val limit = entitlement.effectiveDeviceLimit
        if (limit <= 0) return null
        return "Устройства: ${entitlement.slotsUsed} из $limit"
    }
}

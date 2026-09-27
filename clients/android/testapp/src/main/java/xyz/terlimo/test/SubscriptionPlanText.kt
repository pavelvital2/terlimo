package xyz.terlimo.test

/**
 * Display-only tariff line for the Subscription screen (S5 §07.1).
 *
 * The name is shown only from the SAME accepted `/me` entitlement's authoritative additive
 * `plan.title`, and only while the entitlement is an active right. It is never derived from
 * the plans/quote offers, the opaque `source_ref` or the catalog; null/absent/blank titles
 * produce no line so no plan name is invented, and expired/revoked/unknown_review rights are
 * not labeled as active. Display only: no gate, write, fetch or eligibility.
 */
internal object SubscriptionPlanText {
    fun line(projection: AccountAccessProjection?): String? {
        val entitlement = projection?.entitlement ?: return null
        if (entitlement.status != "active") return null
        val title = entitlement.plan?.title?.takeIf { it.isNotBlank() } ?: return null
        return "Тариф: $title"
    }
}

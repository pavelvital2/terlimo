package xyz.terlimo.test

/**
 * Display-only policy for the visible subscription status on the "Подписка" screen.
 *
 * It never gates access: with an accepted account_access snapshot it mirrors
 * [AccountAccessPolicy.statusLine] (finite remaining, perpetual without a countdown,
 * no access, expired); without a snapshot it shows a neutral pending text and invents
 * no tariff, term, right or technical transport detail. The optional confirmed catalog
 * summary line is kept when it exists.
 */
internal object SubscriptionStatusText {
    /** Shown before the first accepted account_access snapshot. */
    const val PENDING = "Ожидание подтверждённого статуса…"

    fun status(summary: CatalogSummary?, accountAccess: AccountAccessSnapshot?, nowElapsed: Long): String {
        val access = accountAccess?.let { AccountAccessPolicy.statusLine(it, nowElapsed) } ?: PENDING
        return listOfNotNull(summary?.description(), access).joinToString("\n")
    }
}

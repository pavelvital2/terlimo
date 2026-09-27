package xyz.terlimo.test

/**
 * Display-only ordering for the single flattened list shown after a completed "Пинг всех"
 * (S5 §07.2). Successful measured probes come first ascending by the reported probe time,
 * then timeouts, failures and unprobeable nodes with their honest status. Ties are stable
 * by country, name and node id. It never fabricates a value, never changes selection and
 * never resets the chosen gateway; the metric is the measured probe duration reported by
 * native (`transport_setup_ms`), which is a probe/setup time and NOT a network RTT.
 */
internal object CatalogSort {
    fun ranked(cards: List<CatalogCard>): List<CatalogCard> = cards.sortedWith(
        compareBy<CatalogCard> { rank(it) }
            .thenBy { it.rttMs?.takeIf { ms -> ms >= 0 } ?: Long.MAX_VALUE }
            .thenBy { it.country }
            .thenBy { it.name }
            .thenBy { it.nodeId },
    )

    // Only an AVAILABLE node WITH a measured echo RTT (rttMs) may lead the list. A node
    // without a measured RTT is honest data but not a measured success, so it sorts after
    // measured RTTs and is never presented as one.
    private fun rank(card: CatalogCard): Int {
        val measuredRtt = card.availability == NodeAvailability.AVAILABLE && (card.rttMs ?: -1L) >= 0L
        return when {
            measuredRtt -> 0
            card.availability == NodeAvailability.AVAILABLE -> 1
            card.availability == NodeAvailability.TIMEOUT -> 2
            card.availability == NodeAvailability.UNAVAILABLE -> 3
            else -> 4
        }
    }
}

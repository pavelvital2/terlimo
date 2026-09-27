package xyz.terlimo.test

/**
 * Display-only notification text helpers (§07.4). The functional VPN notification must name
 * the selected server so the user can tell which gateway the live traffic belongs to.
 * Bounded and secret-free: only the already-displayed catalog name is used.
 */
internal object NotificationText {
    private const val MAX_NAME = 48

    fun serverLabel(name: String?): String? {
        val value = name?.trim()?.takeIf { it.isNotEmpty() }?.take(MAX_NAME) ?: return null
        return "Сервер: $value"
    }

    /**
     * Active-connection traffic line. For a Connected phase the incoming/outgoing totals are
     * always shown, including an honest zero (the session is live and has not moved traffic),
     * while every other phase carries no traffic segment.
     */
    fun trafficSegment(phase: String, traffic: TrafficSnapshot): String? =
        if (phase != "Connected") null
        else "↓${TrafficText.bytes(traffic.rxTotal)} ↑${TrafficText.bytes(traffic.txTotal)}"
}

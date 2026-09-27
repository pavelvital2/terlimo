package xyz.terlimo.test

import java.util.Locale

/**
 * Display-only formatting for §07.4 session traffic. It never fabricates a measured value:
 * with `measured=false` the rate is an honest unknown and only the last known totals (if
 * any) are shown. Download is RX and upload is TX; units are explicit.
 */
internal object TrafficText {
    fun bytes(value: Long): String {
        val v = value.coerceAtLeast(0)
        return when {
            v < 1024 -> "$v Б"
            v < 1024 * 1024 -> String.format(Locale.ROOT, "%.1f КБ", v / 1024.0)
            v < 1024L * 1024 * 1024 -> String.format(Locale.ROOT, "%.1f МБ", v / (1024.0 * 1024))
            else -> String.format(Locale.ROOT, "%.2f ГБ", v / (1024.0 * 1024 * 1024))
        }
    }

    fun rate(bytesPerSecond: Long): String {
        val v = bytesPerSecond.coerceAtLeast(0)
        return when {
            v < 1024 -> "$v Б/с"
            v < 1024 * 1024 -> String.format(Locale.ROOT, "%.1f КБ/с", v / 1024.0)
            else -> String.format(Locale.ROOT, "%.1f МБ/с", v / (1024.0 * 1024))
        }
    }

    /** Totals line: last known totals are kept even when the current sample is not measured. */
    fun trafficLine(traffic: TrafficSnapshot): String =
        "↓ ${bytes(traffic.rxTotal)} · ↑ ${bytes(traffic.txTotal)}"

    /** Rate line: an unmeasured sample is an honest unknown, never a fabricated zero. */
    fun speedLine(traffic: TrafficSnapshot): String =
        if (!traffic.measured) "↓ — · ↑ —" else "↓ ${rate(traffic.rxRateBps)} · ↑ ${rate(traffic.txRateBps)}"
}

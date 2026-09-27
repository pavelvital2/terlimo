package xyz.terlimo.test

/**
 * Single short instruction under the routing mode selector (S5 §15.1). Exactly one text is
 * shown at a time and it always follows the currently selected mode, on the initial render
 * (including a saved mode) and on every selector change. Display-only: it never touches the
 * selection, the save gate or the routing policy.
 */
internal object RoutingModeHint {
    const val ALL = "Весь трафик устройства проходит через TERLIMO."
    const val INCLUDE = "Отметьте приложения, которые должны использовать VPN. Остальные работают напрямую."
    const val EXCLUDE = "Отметьте приложения, которые должны обходить VPN. Остальные используют VPN."

    fun text(mode: AppRoutingMode): String = when (mode) {
        AppRoutingMode.DISABLED -> ALL
        AppRoutingMode.INCLUDE_ONLY -> INCLUDE
        AppRoutingMode.EXCLUDE -> EXCLUDE
    }
}

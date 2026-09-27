package xyz.terlimo.test

/**
 * Declarative five-tab bottom navigation (S5 §07.1). The label/order/target list is the
 * single source of truth shared by the Activity wiring and the focused unit test, so the
 * approved order Главная/Подписка/Маршрутизация/Настройки/Помощь cannot drift silently.
 * Purely structural: it carries no state and never changes a screen.
 */
internal enum class NavTarget { HOME, SUBSCRIPTION, ROUTING, SETTINGS, HELP }

internal data class NavDestination(val label: String, val iconRes: Int, val target: NavTarget)

internal object BottomNavigation {
    val destinations: List<NavDestination> = listOf(
        NavDestination("Главная", R.drawable.ic_nav_home, NavTarget.HOME),
        NavDestination("Подписка", R.drawable.ic_nav_subscription, NavTarget.SUBSCRIPTION),
        NavDestination("Маршрутизация", R.drawable.ic_nav_routing, NavTarget.ROUTING),
        NavDestination("Настройки", R.drawable.ic_nav_settings, NavTarget.SETTINGS),
        NavDestination("Помощь", R.drawable.ic_nav_help, NavTarget.HELP),
    )

    /** Targets that own an in-place surface inside MainActivity (everything but Routing). */
    private val IN_PLACE = setOf(NavTarget.HOME, NavTarget.SUBSCRIPTION, NavTarget.SETTINGS, NavTarget.HELP)

    /**
     * Selection state of the bottom bar. [visible] is the in-place surface currently shown;
     * [selected] is the highlighted tab. Routing opens its own Activity and therefore never
     * becomes [visible]/[selected]: tapping it keeps the real in-place tab highlighted, so
     * returning with Back shows the same tab that is actually on screen.
     */
    data class NavState(val visible: NavTarget = NavTarget.HOME, val selected: NavTarget = NavTarget.HOME)

    fun initial(): NavState = NavState()

    fun isInPlace(target: NavTarget): Boolean = target in IN_PLACE

    fun onTap(state: NavState, tapped: NavTarget): NavState =
        if (tapped in IN_PLACE) NavState(visible = tapped, selected = tapped) else state

    /**
     * Label of one bottom-navigation tab with the §11 unread red dot applied. Only the Help
     * tab carries the announcement indicator; every other label is unchanged, so the
     * visible tab button itself reflects the unread state (scenario §28).
     */
    fun unreadLabel(label: String, target: NavTarget, unread: Int): String =
        if (target == NavTarget.HELP && unread > 0) "$label ●" else label

    fun tabLabel(destination: NavDestination, unread: Int): String =
        unreadLabel(destination.label, destination.target, unread)
}

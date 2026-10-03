package xyz.terlimo.test

/**
 * Declarative five-tab bottom navigation (S5 §07.1). The label/order/target list is the
 * single source of truth shared by the Activity wiring and the focused unit test, so the
 * approved order Главная/Подписка/Пригласить друга/Настройки/Помощь cannot drift silently.
 * Purely structural: it carries no state and never changes a screen.
 */
internal enum class NavTarget { HOME, SUBSCRIPTION, REFERRAL, SETTINGS, HELP }

internal data class NavDestination(val label: String, val iconRes: Int, val target: NavTarget)

internal object BottomNavigation {
    val destinations: List<NavDestination> = listOf(
        NavDestination("Главная", R.drawable.ic_nav_home, NavTarget.HOME),
        NavDestination("Подписка", R.drawable.ic_nav_subscription, NavTarget.SUBSCRIPTION),
        NavDestination("Пригласить друга", R.drawable.ic_nav_gift, NavTarget.REFERRAL),
        NavDestination("Настройки", R.drawable.ic_nav_settings, NavTarget.SETTINGS),
        NavDestination("Помощь", R.drawable.ic_nav_help, NavTarget.HELP),
    )

    /** All five destinations own an in-place surface; routing is a child of Settings. */
    private val IN_PLACE = NavTarget.entries.toSet()

    /** The highlighted tab always follows its visible surface. */
    data class NavState(val visible: NavTarget = NavTarget.HOME, val selected: NavTarget = NavTarget.HOME)

    fun initial(): NavState = NavState()

    fun isInPlace(target: NavTarget): Boolean = target in IN_PLACE

    /** Restore the parent tab across recreation and a child Activity. */
    fun restoreSelection(state: NavState, restored: NavTarget?): NavState =
        if (restored != null && isInPlace(restored)) NavState(visible = restored, selected = restored) else state

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

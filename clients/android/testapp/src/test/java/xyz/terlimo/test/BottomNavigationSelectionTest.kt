package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Selection behaviour of the five-tab bar: Routing opens its own Activity, so it must not
 * become the visible/selected in-place tab; every other tab is selected by the surface that
 * is actually shown, so returning from Routing with Back keeps the real tab highlighted.
 */
class BottomNavigationSelectionTest {
    @Test
    fun `routing never steals the visible or selected in-place tab`() {
        val initial = BottomNavigation.initial()
        assertEquals(NavTarget.HOME, initial.visible)
        assertEquals(NavTarget.HOME, initial.selected)

        val subscription = BottomNavigation.onTap(initial, NavTarget.SUBSCRIPTION)
        assertEquals(NavTarget.SUBSCRIPTION, subscription.visible)
        assertEquals(NavTarget.SUBSCRIPTION, subscription.selected)

        val routing = BottomNavigation.onTap(subscription, NavTarget.ROUTING)
        assertEquals(NavTarget.SUBSCRIPTION, routing.visible)
        assertEquals(NavTarget.SUBSCRIPTION, routing.selected)

        val settings = BottomNavigation.onTap(routing, NavTarget.SETTINGS)
        assertEquals(NavTarget.SETTINGS, settings.visible)
        assertEquals(NavTarget.SETTINGS, settings.selected)

        val help = BottomNavigation.onTap(settings, NavTarget.HELP)
        assertEquals(NavTarget.HELP, help.visible)
        assertEquals(NavTarget.HELP, help.selected)

        val home = BottomNavigation.onTap(help, NavTarget.HOME)
        assertEquals(NavTarget.HOME, home.visible)
        assertEquals(NavTarget.HOME, home.selected)
    }

    @Test
    fun `only routing is not an in-place surface`() {
        assertTrue(BottomNavigation.isInPlace(NavTarget.HOME))
        assertTrue(BottomNavigation.isInPlace(NavTarget.SUBSCRIPTION))
        assertTrue(BottomNavigation.isInPlace(NavTarget.SETTINGS))
        assertTrue(BottomNavigation.isInPlace(NavTarget.HELP))
        assertFalse(BottomNavigation.isInPlace(NavTarget.ROUTING))
        assertTrue(BottomNavigation.destinations.all {
            BottomNavigation.isInPlace(it.target) || it.target == NavTarget.ROUTING
        })
    }

    @Test
    fun `theme recreate restores an in-place tab with its highlight and never Routing`() {
        val initial = BottomNavigation.initial()
        // Every in-place tab is restored to BOTH visible and selected (the highlight follows).
        for (target in listOf(NavTarget.HOME, NavTarget.SUBSCRIPTION, NavTarget.SETTINGS, NavTarget.HELP)) {
            val restored = BottomNavigation.restoreSelection(initial, target)
            assertEquals(target, restored.visible)
            assertEquals(target, restored.selected)
        }
        // Routing opens its own Activity: restoring it keeps the current in-place state.
        val onSettings = BottomNavigation.onTap(initial, NavTarget.SETTINGS)
        assertEquals(onSettings, BottomNavigation.restoreSelection(onSettings, NavTarget.ROUTING))
        // A cold start (nothing to restore) keeps the current state unchanged.
        assertEquals(initial, BottomNavigation.restoreSelection(initial, null))
    }
}

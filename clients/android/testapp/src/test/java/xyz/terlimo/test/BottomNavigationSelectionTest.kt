package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Test

class BottomNavigationSelectionTest {
    @Test fun `all five surfaces select and survive recreation`() {
        for (target in NavTarget.entries) {
            val selected = BottomNavigation.onTap(BottomNavigation.initial(), target)
            assertEquals(target, selected.visible)
            assertEquals(target, selected.selected)
            assertEquals(selected, BottomNavigation.restoreSelection(BottomNavigation.initial(), target))
        }
    }
    @Test fun `missing saved selection preserves Settings parent`() {
        val parent = BottomNavigation.onTap(BottomNavigation.initial(), NavTarget.SETTINGS)
        assertEquals(parent, BottomNavigation.restoreSelection(parent, null))
    }
}

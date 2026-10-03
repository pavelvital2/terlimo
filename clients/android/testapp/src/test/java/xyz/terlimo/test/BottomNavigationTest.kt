package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class BottomNavigationTest {
    @Test
    fun `five tabs in the approved order with the approved targets`() {
        val destinations = BottomNavigation.destinations
        assertEquals(
            listOf("Главная", "Подписка", "Пригласить друга", "Настройки", "Помощь"),
            destinations.map { it.label },
        )
        assertEquals(
            listOf(NavTarget.HOME, NavTarget.SUBSCRIPTION, NavTarget.REFERRAL, NavTarget.SETTINGS, NavTarget.HELP),
            destinations.map { it.target },
        )
    }

    @Test
    fun `every tab has an icon and no duplicate labels`() {
        val destinations = BottomNavigation.destinations
        assertTrue(destinations.all { it.iconRes != 0 })
        assertEquals(destinations.size, destinations.map { it.label }.toSet().size)
    }
}

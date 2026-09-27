package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class RoutingRuntimePlanTest {
    private val self = setOf("xyz.terlimo.test")
    private fun doc(mode: AppRoutingMode = AppRoutingMode.EXCLUDE, packages: Set<String> = self) =
        RoutingSettingsDocument(revision = 1, routing = RoutingPolicy(AppRoutingPolicy(mode, packages)))

    @Test fun disabledAndExcludeKeepDefaultRouteFromSelectedVpn() {
        val plan = RoutingRuntimePlanner.plan(doc(), listOf("1.1.1.1"), setOf("xyz.terlimo.test"), self)
        assertEquals(listOf("0.0.0.0/0"), plan.ipv4Routes)
        assertTrue(plan.excludedApplications.isEmpty())
    }

    @Test fun dnsRequiresAuthenticatedVpnConfigValuesAndIpv4() {
        val value = doc()
        assertEquals(listOf("9.9.9.9"), RoutingRuntimePlanner.plan(value,
            listOf("9.9.9.9"), setOf("xyz.terlimo.test"), self).dnsServers)
        assertThrows(IllegalArgumentException::class.java) { RoutingRuntimePlanner.plan(value, emptyList(), emptySet(), self) }
    }

    @Test fun emptyInstalledIncludeOnlyIsFailClosed() {
        val value = doc(AppRoutingMode.INCLUDE_ONLY, setOf("com.example.missing"))
        val plan = RoutingRuntimePlanner.plan(value, listOf("1.1.1.1"), emptySet(), self)
        assertTrue(plan.includedApplications.isEmpty())
        assertTrue(plan.ipv4Routes.isEmpty())
    }
}

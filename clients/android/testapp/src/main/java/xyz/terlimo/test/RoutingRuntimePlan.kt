package xyz.terlimo.test

import java.net.InetAddress

internal data class RoutingRuntimePlan(
    val includedApplications: List<String>,
    val excludedApplications: List<String>,
    val ipv4Routes: List<String>,
    val dnsServers: List<String>,
)

internal object RoutingRuntimePlanner {
    fun plan(
        document: RoutingSettingsDocument,
        vpnConfigDns: List<String>,
        installedPackages: Set<String>,
        protectedPackages: Set<String>,
    ): RoutingRuntimePlan {
        val app = document.routing.apps
        // Self/protected packages are never part of the effective OS allow/disallow set
        // derived from user selection. A stale legacy self entry is dropped here too.
        val protectedInstalled = protectedPackages.map(RoutingPolicyNormalizer::packageName)
            .filter { it in installedPackages }.toSet()
        val present = app.packageNames.intersect(installedPackages).filterNot { it in protectedInstalled }
        val included = if (app.mode == AppRoutingMode.INCLUDE_ONLY) present.sorted() else emptyList()
        val excluded = if (app.mode == AppRoutingMode.EXCLUDE) present.sorted() else emptyList()
        val dns = vpnConfigDns.map(::requireIpv4).also { require(it.isNotEmpty()) { "VPN_CONFIG_DNS_MISSING" } }
        // With no allowed applications Android interprets the builder as all apps.
        // Keep INCLUDE_ONLY empty fail-closed by installing no routes at all.
        val routes = if (app.mode == AppRoutingMode.INCLUDE_ONLY && included.isEmpty()) emptyList()
        else listOf("0.0.0.0/0")
        // INCLUDE_ONLY switches the builder to an allow-list, so self/protected must be
        // listed explicitly to keep readiness inside the VPN. An empty user selection stays
        // fail-closed: no allow entries and no routes.
        val effectiveIncluded = if (app.mode == AppRoutingMode.INCLUDE_ONLY && included.isNotEmpty())
            (included + protectedInstalled).sorted() else included
        return RoutingRuntimePlan(effectiveIncluded, excluded, routes, dns)
    }

    private fun requireIpv4(value: String): String {
        val parts = value.split('.')
        require(parts.size == 4) { "VPN_CONFIG_DNS_INVALID" }
        val bytes = ByteArray(4)
        parts.forEachIndexed { index, part ->
            require(part.isNotEmpty() && part.length <= 3 && part.all(Char::isDigit) &&
                (part.length == 1 || !part.startsWith('0'))) { "VPN_CONFIG_DNS_INVALID" }
            val octet = part.toIntOrNull() ?: throw IllegalArgumentException("VPN_CONFIG_DNS_INVALID")
            require(octet in 0..255) { "VPN_CONFIG_DNS_INVALID" }
            bytes[index] = octet.toByte()
        }
        return requireNotNull(InetAddress.getByAddress(bytes).hostAddress)
    }
}

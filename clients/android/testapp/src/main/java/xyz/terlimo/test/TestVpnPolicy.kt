package xyz.terlimo.test

import java.net.Inet4Address
import java.net.InetAddress

internal object TestVpnPolicy {
    fun requireIpv4(addresses: Collection<InetAddress>, dns: Collection<InetAddress>, routes: Collection<InetAddress>) {
        require(addresses.isNotEmpty() && dns.isNotEmpty() && routes.isNotEmpty()) { "VPN_CONFIG_INVALID" }
        require((addresses + dns + routes).all { it is Inet4Address }) { "VPN_IPV6_UNSUPPORTED" }
    }
    fun ready(rx: Long, tx: Long, handshake: Long, applyStarted: Long, now: Long): Boolean =
        rx > 0 && tx > 0 && handshake >= applyStarted && handshake <= now
}

package xyz.terlimo.test;

import com.wireguard.config.InetNetwork;
import java.net.InetAddress;
import java.util.List;

/** Fully validated immutable input to the VpnService.Builder adapter. */
final class RoutingVpnPlan {
    final List<String> includedApplications;
    final List<String> excludedApplications;
    final List<InetNetwork> routes;
    final List<InetAddress> dnsServers;

    RoutingVpnPlan(List<String> included, List<String> excluded, List<InetNetwork> routes, List<InetAddress> dns) {
        this.includedApplications = List.copyOf(included);
        this.excludedApplications = List.copyOf(excluded);
        this.routes = List.copyOf(routes);
        this.dnsServers = List.copyOf(dns);
    }
}

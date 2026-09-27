package xyz.terlimo.test

/**
 * Host-side adapter that feeds the accepted per-grant user-data peer counters into the pure
 * [TrafficAccounting] D06 model. It owns only the host monotonic sequence and the session
 * lifecycle calls; it does not read transport/relay diagnostics, does not touch the VPN and
 * performs no display/formatting.
 *
 * The counters come from the active WireGuard peer (single peer of the one active tunnel):
 * peer `rxBytes` = user download, peer `txBytes` = user upload, cumulative since the peer
 * was created. A new explicit Connect creates a new peer/tunnel (epoch reset); recovery and
 * in-place switch keep the accumulated totals and only re-baseline.
 */
internal class TrafficSampler(private val accounting: TrafficAccounting = TrafficAccounting()) {
    private var sequence = 0L

    fun onExplicitConnect(): TrafficSnapshot {
        sequence = 0
        return accounting.onExplicitConnect()
    }

    fun onRecoveryOrSwitch(): TrafficSnapshot = accounting.onRecoveryOrSwitch()

    fun onDisconnect(): TrafficSnapshot = accounting.onDisconnect()

    fun onPeerSample(rxBytes: Long, txBytes: Long, monotonicMs: Long, epoch: String): TrafficSnapshot {
        sequence++
        return accounting.onSample(TrafficSample(rxBytes, txBytes, monotonicMs, epoch, sequence))
    }

    fun snapshot(): TrafficSnapshot = accounting.snapshot()
}

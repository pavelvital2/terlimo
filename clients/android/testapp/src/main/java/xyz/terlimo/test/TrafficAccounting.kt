package xyz.terlimo.test

/**
 * Pure D06 session-traffic accounting for §07.4 (S5).
 *
 * INPUT CONTRACT: only the **per-grant user-data** counters reported by the node (the one
 * agreed accounting layer) plus a monotonic time and a source epoch/sequence. Transport
 * diagnostic counters (UDP/relay/WG-internal) MUST NOT be fed here for production display.
 *
 * Rules implemented:
 *  - an explicit Connect after OFF starts the user session and resets totals/rate;
 *  - internal recovery/switch only changes the baseline, never losing the accumulated sum;
 *  - a negative delta (or a changed source epoch) starts a new source epoch and never
 *    produces negative traffic;
 *  - rate = delta(real bytes) / delta(monotonic time);
 *  - stale/foreign samples (older sequence, foreign epoch) never double-count.
 *
 * Display/formatting is out of scope; this type only owns truth-preserving arithmetic.
 */
internal data class TrafficSample(
    val rxBytes: Long,
    val txBytes: Long,
    val monotonicMs: Long,
    val sourceEpoch: String,
    val sequence: Long,
)

internal data class TrafficSnapshot(
    val sessionId: Long = 0,
    val active: Boolean = false,
    val rxTotal: Long = 0,
    val txTotal: Long = 0,
    val rxRateBps: Long = 0,
    val txRateBps: Long = 0,
    val sourceEpoch: String = "",
    // True only after a real per-grant sample was applied. A lifecycle transition or a
    // missing/ambiguous sample keeps the accumulated totals but clears this flag so a
    // display never presents a fabricated zero as a measured value.
    val measured: Boolean = false,
)

internal class TrafficAccounting {
    private var sessionId = 0L
    private var active = false
    private var epoch = ""
    private var lastSequence = Long.MIN_VALUE
    private var baselineRx = 0L
    private var baselineTx = 0L
    private var baselineMs = 0L
    private var awaitingBaseline = true
    // A brand-new explicit Connect creates a fresh WG peer whose counters start at zero, so
    // the first sample after Connect may be credited in full instead of only baselined.
    private var countFromZero = false
    // Fences against late/foreign callbacks: a retired epoch can never become current again,
    // and any sample whose monotonic time does not advance is ignored.
    private val retiredEpochs = mutableSetOf<String>()
    private var lastAcceptedMs = Long.MIN_VALUE
    private var rxTotal = 0L
    private var txTotal = 0L
    private var rxRate = 0L
    private var txRate = 0L
    private var measured = false

    fun snapshot(): TrafficSnapshot =
        TrafficSnapshot(sessionId, active, rxTotal, txTotal, rxRate, txRate, epoch, measured)

    /** Explicit user Connect after OFF: a brand-new user session, counters reset. */
    fun onExplicitConnect(): TrafficSnapshot {
        sessionId++
        active = true
        epoch = ""
        retiredEpochs.clear()
        lastSequence = Long.MIN_VALUE
        lastAcceptedMs = Long.MIN_VALUE
        awaitingBaseline = true
        countFromZero = true
        rxTotal = 0
        txTotal = 0
        rxRate = 0
        txRate = 0
        measured = false
        return snapshot()
    }

    /** Internal recovery/switch: keep accumulated totals, re-baseline on the next sample. */
    fun onRecoveryOrSwitch(): TrafficSnapshot {
        if (active) {
            awaitingBaseline = true
            countFromZero = false
            rxRate = 0
            txRate = 0
            measured = false
        }
        return snapshot()
    }

    /** Explicit Disconnect ends the user session; totals stay as the final accounted sum. */
    fun onDisconnect(): TrafficSnapshot {
        active = false
        rxRate = 0
        txRate = 0
        measured = false
        return snapshot()
    }

    /** Applies one per-grant user-data sample. Returns the updated snapshot. */
    fun onSample(sample: TrafficSample): TrafficSnapshot {
        if (!active) return snapshot()
        if (sample.rxBytes < 0 || sample.txBytes < 0 || sample.monotonicMs < 0) return snapshot()

        // Monotonic fence: a sample that does not advance the clock is late/reordered.
        if (sample.monotonicMs <= lastAcceptedMs) return snapshot()

        val sourceChanged = sample.sourceEpoch != epoch
        if (sourceChanged) {
            // A retired epoch is a late callback from a source we already moved past: it can
            // never replace the current baseline.
            if (sample.sourceEpoch in retiredEpochs) return snapshot()
            if (epoch.isNotEmpty()) retiredEpochs.add(epoch)
            epoch = sample.sourceEpoch
            lastSequence = Long.MIN_VALUE
            awaitingBaseline = true
        } else if (sample.sequence <= lastSequence) {
            // Stale same-epoch sample is rejected even while awaiting a new baseline.
            return snapshot()
        }
        if (awaitingBaseline) {
            if (countFromZero) {
                // New peer starts at zero at Connect: credit the bytes accrued before the
                // first tick; there is no earlier monotonic point, so rate stays 0.
                rxTotal += sample.rxBytes
                txTotal += sample.txBytes
                rxRate = 0
                txRate = 0
                countFromZero = false
            }
            rebaseline(sample)
            return snapshot()
        }
        val deltaRx = sample.rxBytes - baselineRx
        val deltaTx = sample.txBytes - baselineTx
        if (deltaRx < 0 || deltaTx < 0) {
            // Raw counter reset inside the same source epoch: re-baseline, never negative.
            lastSequence = Long.MIN_VALUE
            rebaseline(sample)
            return snapshot()
        }
        val deltaMs = sample.monotonicMs - baselineMs
        rxTotal += deltaRx
        txTotal += deltaTx
        if (deltaMs > 0) {
            rxRate = deltaRx * 1000 / deltaMs
            txRate = deltaTx * 1000 / deltaMs
        } else {
            rxRate = 0
            txRate = 0
        }
        baselineRx = sample.rxBytes
        baselineTx = sample.txBytes
        baselineMs = sample.monotonicMs
        lastSequence = sample.sequence
        lastAcceptedMs = sample.monotonicMs
        measured = true
        return snapshot()
    }

    private fun rebaseline(sample: TrafficSample) {
        baselineRx = sample.rxBytes
        baselineTx = sample.txBytes
        baselineMs = sample.monotonicMs
        lastSequence = sample.sequence
        lastAcceptedMs = sample.monotonicMs
        awaitingBaseline = false
        measured = true
    }
}

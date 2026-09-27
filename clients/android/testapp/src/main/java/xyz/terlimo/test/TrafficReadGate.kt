package xyz.terlimo.test

/**
 * Monotonic generation token that fences asynchronous traffic reads (§07.4).
 *
 * A blocking `getStatistics` read is scheduled with the value of [current]; the callback is
 * applied only when [accepts] still holds. The token is bumped on every explicit Connect,
 * recovery/switch baseline transition, Disconnect/stop and whenever the sampler is disarmed
 * on phase loss, so a late callback from an old tunnel can never mutate the new session even
 * when the attempt id, node id and phase happen to return to the same values.
 */
internal class TrafficReadGate {
    private var generation = 0L

    fun bump(): Long {
        generation++
        return generation
    }

    fun current(): Long = generation

    fun accepts(captured: Long): Boolean = captured == generation
}

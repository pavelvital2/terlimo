package xyz.terlimo.test

import java.util.concurrent.ArrayBlockingQueue
import java.util.concurrent.ThreadPoolExecutor
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean

/** One actor, bounded BEFORE dispatch/signing admission. No caller-runs fallback. */
internal class BridgeActor {
    enum class Result { ACCEPTED, FULL, CLOSED }
    private val closed = AtomicBoolean(false)
    // Four admitted signing requests + four persist/VPN/lease/control slots.
    // These are shared slots, not reserved per type; overflow never loses an ACK silently.
    private val executor = ThreadPoolExecutor(1, 1, 0, TimeUnit.MILLISECONDS,
        ArrayBlockingQueue<Runnable>(CAPACITY)).apply { prestartCoreThread() }

    fun submit(waitMillis: Long = 0, action: () -> Unit): Result {
        require(waitMillis in 0..BACKPRESSURE_MS)
        val task = Runnable { if (!closed.get()) action() }
        val deadline = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(waitMillis)
        do {
            if (closed.get()) return Result.CLOSED
            val remaining = deadline - System.nanoTime()
            val offered = try {
                if (remaining <= 0) executor.queue.offer(task)
                else executor.queue.offer(task, minOf(remaining, TimeUnit.MILLISECONDS.toNanos(50)), TimeUnit.NANOSECONDS)
            } catch (_: InterruptedException) {
                Thread.currentThread().interrupt()
                return if (closed.get()) Result.CLOSED else Result.FULL
            }
            if (offered) {
                // close may have drained the queue concurrently. Never claim a live admission then.
                if (closed.get()) { executor.remove(task); return Result.CLOSED }
                return Result.ACCEPTED
            }
        } while (System.nanoTime() < deadline)
        return if (closed.get()) Result.CLOSED else Result.FULL
    }

    /** Cancel bypasses the queue; a waiting reader wakes within 50ms. */
    fun close() {
        if (closed.compareAndSet(false, true)) executor.shutdownNow()
    }

    // Only teardown uses this AFTER native/VPN abort. A running durable write
    // must finish before another Service instance is allowed to start a writer.
    fun awaitStopped(timeoutMillis: Long): Boolean = executor.awaitTermination(timeoutMillis, TimeUnit.MILLISECONDS)

    companion object {
        const val CAPACITY = 8
        const val BACKPRESSURE_MS = 1000L // Below native persist(10s)/sign(15s) budgets.
        fun overflowCode(type: String): String = when (type) {
            "sign" -> "BRIDGE_SIGN_OVERFLOW"
            "persist" -> "BRIDGE_PERSIST_OVERFLOW"
            "vpn_config" -> "BRIDGE_VPN_OVERFLOW"
            else -> "BRIDGE_INGRESS_OVERFLOW"
        }
    }
}

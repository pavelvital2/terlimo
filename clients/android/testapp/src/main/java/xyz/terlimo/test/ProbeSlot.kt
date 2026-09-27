package xyz.terlimo.test

import java.util.concurrent.Future
import java.util.concurrent.SynchronousQueue
import java.util.concurrent.ThreadPoolExecutor
import java.util.concurrent.TimeUnit

/** A blocked platform DNS call cannot create an unbounded sequence of probe threads. */
internal class ProbeSlot : AutoCloseable {
    private val executor = ThreadPoolExecutor(1, 1, 0, TimeUnit.MILLISECONDS, SynchronousQueue(),
        { action -> Thread(action, "terlimo-probe").apply { isDaemon = true } })
    fun submit(action: () -> Unit): Future<*> = executor.submit(action)
    override fun close() { executor.shutdownNow() }
}

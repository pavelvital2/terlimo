package xyz.terlimo.test

import java.util.concurrent.atomic.AtomicBoolean

/**
 * STEP03.6 owner contract 4: on the first MainActivity render of the process the standard
 * linkless bootstrap/resume runs once, so `/me` and the gateways load without tapping
 * «Открыть сохранённую подписку». The claim is process-scoped (companion object) and
 * one-shot; an Activity recreation must not start a second Service command.
 */
internal object ProcessAutoLoad {
    private val pending = AtomicBoolean(true)

    fun claim(): Boolean = pending.compareAndSet(true, false)
}

/**
 * The auto-load is never conditioned on the cached list: a saved verified list must not
 * cancel the status refresh (the refresh itself re-reads the server state through the
 * existing resume path). Only the return of [ProcessAutoLoad.claim] bounds it.
 */
internal object AutoLoadPolicy {
    fun shouldStart(cachedNodes: List<NodeLabel>): Boolean = true
}

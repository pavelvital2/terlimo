package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test
import java.util.ArrayDeque

/** The same immediate-invalidation / deferred-cleanup adapter used by SessionService. */
class CatalogRefreshCancellationTest {
    private val gate = CatalogRefreshCoordinator()
    private val observers = CatalogRefreshRequestRegistry()
    private val queue = ArrayDeque<() -> Unit>()
    private val stopped = mutableListOf<String>()
    private val cancellation = CatalogRefreshCancellation(gate, observers,
        { queue.addLast(it) }, { ids -> ids.filterTo(stopped) { gate.isJobOnly(it) } })
    private var callbacks = 0

    private fun register() {
        gate.setMode(CatalogRefreshMode.DAILY)
        assertTrue(gate.registerJob("job", gate.currentEpoch()))
        assertTrue(observers.register("job", object : CatalogRefreshObserver {
            override fun onCompleted(requestId: String, ok: Boolean, error: String?) { callbacks++ }
        }))
    }
    private fun drain() { while (queue.isNotEmpty()) queue.removeFirst().invoke() }

    @Test fun stopInvalidatesBeforeTheQueuedColdBegin() {
        register()
        assertTrue(gate.pendCold("job"))
        var origin: CatalogRefreshCoordinator.Origin? = null
        queue.addLast { origin = gate.beginCycle("attempt", "job") }
        cancellation.cancel("job")
        drain()
        assertNull(origin)
        assertFalse(gate.isJobOnly("attempt"))
        observers.deliver("job", true, null)
        assertEquals(0, callbacks)
    }

    @Test fun stopInvalidatesBeforeTheQueuedActiveSend() {
        register()
        gate.beginCycle("manual")
        var sends = 0
        queue.addLast { gate.dispatch("manual", "job") { sends++; true } }
        cancellation.cancel("job")
        drain()
        assertEquals(0, sends)
        assertTrue(stopped.isEmpty())
        assertFalse(gate.attach("manual", "job"))
    }

    @Test fun cancelBetweenColdAdmissionAndNativeStartSuppressesStartAndPublication() {
        register()
        assertTrue(gate.pendCold("job"))
        assertNotNull(gate.beginCycle("attempt", "job"))
        cancellation.cancel("job")
        assertFalse(gate.dispatch("attempt", "job") { error("must not start native") })
        assertFalse(gate.commit("attempt") { error("must not publish") }.publish)
        drain()
        assertEquals(listOf("attempt"), stopped)
    }

    @Test fun manualTakeoverBeforeDeferredCleanupKeepsTheAttempt() {
        register()
        gate.pendCold("job")
        gate.beginCycle("attempt", "job")
        cancellation.cancel("job")
        gate.markAttemptManual("attempt")
        drain()
        assertTrue(stopped.isEmpty())
        assertTrue(gate.commit("attempt") {}.publish)
    }

    @Test fun unknownOrRepeatedCancellationCannotStopAnotherJob() {
        register()
        gate.pendCold("job")
        gate.beginCycle("attempt", "job")
        cancellation.cancel("unrelated")
        drain()
        assertTrue(stopped.isEmpty())
        assertTrue(gate.dispatch("attempt", "job") { true })
        cancellation.cancel("job")
        cancellation.cancel("job")
        drain()
        assertEquals(listOf("attempt"), stopped)
    }

    @Test fun offBetweenAdmissionAndSendCannotDispatchEvenAfterReenable() {
        register()
        gate.pendCold("job")
        gate.beginCycle("attempt", "job")
        gate.setMode(CatalogRefreshMode.OFF)
        gate.setMode(CatalogRefreshMode.DAILY)
        assertFalse(gate.dispatch("attempt", "job") { error("stale send") })
    }
}

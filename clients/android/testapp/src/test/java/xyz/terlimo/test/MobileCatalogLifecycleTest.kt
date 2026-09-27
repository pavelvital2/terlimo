package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test
import java.util.TreeMap

/**
 * Execution tests of the real per-attempt state machine and the real finite window timer.
 *
 * The host below mirrors SessionService wiring exactly: the same [MobileCatalogGate] and
 * [CatalogDeadlineTimer] objects are driven through the same event sequences, with the timer's
 * scheduler replaced by a deterministic virtual clock (no sleeps, no wall clock).
 */
class MobileCatalogLifecycleTest {
    /** Virtual clock: `now` only moves through [advanceBy]; scheduled callbacks fire in time order. */
    private class VirtualClock {
        var now = 0L
            private set
        var scheduled = 0
            private set
        private val tasks = TreeMap<Long, MutableList<() -> Unit>>()

        fun schedule(delayMillis: Long, callback: () -> Unit) {
            scheduled++
            tasks.getOrPut(now + delayMillis) { mutableListOf() }.add(callback)
        }

        fun advanceBy(millis: Long) {
            val target = now + millis
            while (true) {
                val next = tasks.firstEntry() ?: break
                if (next.key > target) break
                tasks.remove(next.key)
                now = next.key
                next.value.forEach { it() }
            }
            now = target
        }
    }

    /** Real gate + real timer; the stop mapping mirrors SessionService.stopAttempt(). */
    private class MobileAttemptHost(val clock: VirtualClock) {
        val gate = MobileCatalogGate()
        val timeouts = mutableListOf<Pair<Long, String>>()
        val timer = CatalogDeadlineTimer(
            schedule = clock::schedule,
            activeAttempt = { activeAttempt },
            onTimeout = { timeouts += clock.now to "CATALOG_TIMEOUT" },
        )
        var activeAttempt: String? = null
            private set

        fun start(attempt: String, mobile: Boolean) {
            activeAttempt = attempt
            timer.apply(attempt, gate.onAttemptStart(attempt, mobile))
        }

        fun accountAccess(attempt: String, dataAccess: String) {
            timer.apply(attempt, gate.onAccountAccess(dataAccess, attempt))
        }

        fun catalogAccepted(attempt: String) {
            timer.apply(attempt, gate.onCatalogAccepted(attempt))
        }

        fun stop(attempt: String?) {
            if (gate.onAttemptStop(attempt) == MobileCatalogAction.DISARM) timer.clear()
            if (attempt == null || attempt == activeAttempt) activeAttempt = null
        }

        fun assertConsistent(attempt: String) {
            assertEquals(
                "timer armed must mirror WAITING_CATALOG for $attempt",
                gate.stateOf(attempt) == MobileCatalogState.WAITING_CATALOG,
                timer.pending(attempt),
            )
        }
    }

    @Test fun rightTransitionArmsOnceAndCatalogAcceptanceIsTerminal() {
        val clock = VirtualClock()
        val host = MobileAttemptHost(clock)
        val a = "attempt-a"

        host.start(a, mobile = true)
        assertEquals(1, clock.scheduled)
        assertEquals(MobileCatalogState.WAITING_CATALOG, host.gate.stateOf(a))
        host.assertConsistent(a)

        clock.advanceBy(1_000)
        host.accountAccess(a, "none")
        assertEquals(MobileCatalogState.WAITING_RIGHT, host.gate.stateOf(a))
        assertFalse(host.timer.pending(a))
        assertEquals("disarm schedules nothing", 1, clock.scheduled)
        host.assertConsistent(a)

        clock.advanceBy(1_000)
        host.accountAccess(a, "subscription_data")
        assertEquals(MobileCatalogState.WAITING_CATALOG, host.gate.stateOf(a))
        assertTrue(host.timer.pending(a))
        assertEquals("exactly one arm on the transition", 2, clock.scheduled)
        host.assertConsistent(a)

        clock.advanceBy(1_000)
        host.catalogAccepted(a)
        assertEquals(MobileCatalogState.CATALOG_ACCEPTED, host.gate.stateOf(a))
        assertFalse(host.timer.pending(a))
        assertEquals("acceptance schedules nothing", 2, clock.scheduled)
        host.assertConsistent(a)

        // Later accepted rights must not re-arm the terminal attempt.
        clock.advanceBy(1_000)
        host.accountAccess(a, "subscription_data")
        host.accountAccess(a, "onboarding_hour")
        assertEquals(2, clock.scheduled)
        assertFalse(host.timer.pending(a))
        clock.advanceBy(60_000)
        assertTrue(host.timeouts.isEmpty())
    }

    @Test fun missingCatalogAfterADataRightTimesOutExactlyFifteenSecondsAfterTheTransition() {
        val clock = VirtualClock()
        val host = MobileAttemptHost(clock)
        val a = "attempt-a"

        host.start(a, mobile = true)
        clock.advanceBy(1_000)
        host.accountAccess(a, "none")
        clock.advanceBy(1_000)
        host.accountAccess(a, "subscription_data")

        clock.advanceBy(13_000)
        assertTrue("the fenced begin window must not cut the fresh one short", host.timeouts.isEmpty())
        clock.advanceBy(1_999)
        assertTrue(host.timeouts.isEmpty())
        clock.advanceBy(1)
        assertEquals(listOf(17_000L to "CATALOG_TIMEOUT"), host.timeouts)
    }

    @Test fun repeatedAcceptedRightsDoNotExtendTheOriginalWindow() {
        val clock = VirtualClock()
        val host = MobileAttemptHost(clock)
        val a = "attempt-a"

        host.start(a, mobile = true)
        clock.advanceBy(1_000)
        host.accountAccess(a, "none")
        clock.advanceBy(1_000)
        host.accountAccess(a, "subscription_data")

        clock.advanceBy(5_000)
        host.accountAccess(a, "subscription_data")
        assertEquals(MobileCatalogState.WAITING_CATALOG, host.gate.stateOf(a))
        clock.advanceBy(5_000)
        host.accountAccess(a, "onboarding_hour")
        assertEquals(MobileCatalogState.WAITING_CATALOG, host.gate.stateOf(a))
        assertEquals("repeated accepted rights are idempotent, no new arm", 2, clock.scheduled)

        clock.advanceBy(4_999)
        assertTrue(host.timeouts.isEmpty())
        clock.advanceBy(1)
        assertEquals(listOf(17_000L to "CATALOG_TIMEOUT"), host.timeouts)
    }

    @Test fun noDataRightKeepsWaitingRightWithoutATimer() {
        val clock = VirtualClock()
        val host = MobileAttemptHost(clock)
        val a = "attempt-a"

        host.start(a, mobile = true)
        clock.advanceBy(1_000)
        host.accountAccess(a, "none")
        assertEquals(MobileCatalogState.WAITING_RIGHT, host.gate.stateOf(a))
        assertFalse(host.timer.pending(a))
        clock.advanceBy(60_000)
        assertTrue("no timer at all while WAITING_RIGHT", host.timeouts.isEmpty())
        host.accountAccess(a, "restricted_checkout")
        host.accountAccess(a, "none")
        host.accountAccess(a, "restricted_checkout")
        assertEquals(MobileCatalogState.WAITING_RIGHT, host.gate.stateOf(a))
        assertFalse(host.timer.pending(a))
        assertTrue(host.timeouts.isEmpty())
        assertEquals("only the begin window ever existed", 1, clock.scheduled)
        host.assertConsistent(a)

        // Not terminal: the same attempt still transitions on a later data right.
        clock.advanceBy(1_000)
        host.accountAccess(a, "subscription_data")
        assertEquals(MobileCatalogState.WAITING_CATALOG, host.gate.stateOf(a))
        assertTrue(host.timer.pending(a))
        assertEquals(2, clock.scheduled)
    }

    @Test fun staleEventsAndCallbacksCannotTouchANewAttempt() {
        val clock = VirtualClock()
        val host = MobileAttemptHost(clock)
        val a = "attempt-a"
        val b = "attempt-b"

        host.start(a, mobile = true)
        clock.advanceBy(1_000)
        host.accountAccess(a, "none")
        clock.advanceBy(4_000)
        host.start(b, mobile = true)
        assertEquals(2, clock.scheduled)

        host.accountAccess(a, "subscription_data")
        host.catalogAccepted(a)
        assertEquals(MobileCatalogState.IDLE, host.gate.stateOf(a))
        assertEquals(MobileCatalogState.WAITING_CATALOG, host.gate.stateOf(b))
        assertTrue(host.timer.pending(b))
        assertEquals("stale events cannot arm", 2, clock.scheduled)

        clock.advanceBy(10_000)
        assertTrue("the fenced old callback must not stop the new attempt", host.timeouts.isEmpty())
        clock.advanceBy(4_999)
        assertTrue(host.timeouts.isEmpty())
        clock.advanceBy(1)
        assertEquals(listOf(20_000L to "CATALOG_TIMEOUT"), host.timeouts)
    }

    @Test fun stopFencesTheOldAttemptAndAStaleStopCannotResetTheCurrentOne() {
        val clock = VirtualClock()
        val host = MobileAttemptHost(clock)
        val a = "attempt-a"
        val b = "attempt-b"

        host.start(a, mobile = true)
        clock.advanceBy(1_000)
        host.catalogAccepted(a)
        clock.advanceBy(1_000)
        host.stop(a)
        assertEquals(MobileCatalogState.IDLE, host.gate.stateOf(a))
        assertFalse(host.timer.pending(a))

        clock.advanceBy(1_000)
        host.start(b, mobile = true)
        assertEquals(2, clock.scheduled)
        host.stop(a)
        assertEquals(MobileCatalogState.WAITING_CATALOG, host.gate.stateOf(b))
        assertTrue("a stale stop cannot clear the current timer", host.timer.pending(b))

        clock.advanceBy(14_999)
        assertTrue(host.timeouts.isEmpty())
        clock.advanceBy(1)
        assertEquals(listOf(18_000L to "CATALOG_TIMEOUT"), host.timeouts)
    }

    @Test fun acceptedCatalogNeverReArms() {
        val clock = VirtualClock()
        val host = MobileAttemptHost(clock)
        val a = "attempt-a"

        host.start(a, mobile = true)
        clock.advanceBy(500)
        host.catalogAccepted(a)
        assertEquals(MobileCatalogState.CATALOG_ACCEPTED, host.gate.stateOf(a))
        assertFalse(host.timer.pending(a))

        clock.advanceBy(1_000)
        host.accountAccess(a, "none")
        host.accountAccess(a, "subscription_data")
        host.accountAccess(a, "onboarding_hour")
        host.catalogAccepted(a)
        assertEquals(MobileCatalogState.CATALOG_ACCEPTED, host.gate.stateOf(a))
        assertFalse(host.timer.pending(a))
        assertEquals(1, clock.scheduled)
        clock.advanceBy(60_000)
        assertTrue(host.timeouts.isEmpty())
    }

    @Test fun legacyAttemptsKeepTheBeginWindowAndIgnoreRights() {
        val clock = VirtualClock()
        val host = MobileAttemptHost(clock)
        val a = "legacy-a"

        host.start(a, mobile = false)
        assertTrue(host.timer.pending(a))
        assertEquals(MobileCatalogState.WAITING_CATALOG, host.gate.stateOf(a))
        clock.advanceBy(1_000)
        host.accountAccess(a, "none")
        host.accountAccess(a, "subscription_data")
        assertTrue("legacy attempts ignore account_access rights", host.timer.pending(a))
        assertEquals(1, clock.scheduled)
        clock.advanceBy(1_000)
        host.catalogAccepted(a)
        assertFalse(host.timer.pending(a))
        assertEquals(MobileCatalogState.CATALOG_ACCEPTED, host.gate.stateOf(a))
        clock.advanceBy(60_000)
        assertTrue(host.timeouts.isEmpty())
    }

    @Test fun legacyAttemptWithoutCatalogTimesOutAtTheExistingBound() {
        val clock = VirtualClock()
        val host = MobileAttemptHost(clock)
        host.start("legacy", mobile = false)
        clock.advanceBy(14_999)
        assertTrue(host.timeouts.isEmpty())
        clock.advanceBy(1)
        assertEquals(listOf(15_000L to "CATALOG_TIMEOUT"), host.timeouts)
    }
}

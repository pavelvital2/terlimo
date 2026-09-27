package xyz.terlimo.test

import java.util.concurrent.CountDownLatch
import java.util.concurrent.atomic.AtomicReference
import org.junit.Assert.*
import org.junit.Test

class ChildLifecycleTest {
    private val attemptA = "11111111-1111-1111-1111-111111111111"
    private val attemptB = "22222222-2222-2222-2222-222222222222"

    @Test fun hostStopIsReportedOnceWithFixedReasonAndPhase() {
        val lifecycle = ChildLifecycle(attemptA, 7L)
        lifecycle.recordHostStop(ChildStopReason.TEARDOWN, "Stopping")
        val first = lifecycle.complete(0)
        val second = lifecycle.complete(0)
        assertNotNull(first)
        assertNull(second)
        assertEquals(attemptA, first!!.attempt)
        assertEquals(7L, first.generation)
        assertEquals(0, first.exitCode)
        assertEquals(ChildObservation.HOST_STOP, first.firstObservation)
        assertTrue(first.closing)
        assertEquals(ChildStopReason.TEARDOWN, first.stop?.reason)
        assertEquals("Stopping", first.stop?.phase)
    }

    @Test fun stdoutEofIsOnlyAnObservationNotHostStop() {
        val lifecycle = ChildLifecycle(attemptA, 1L)
        lifecycle.markObservation(ChildObservation.STDOUT_EOF)
        val completion = lifecycle.complete(9)
        assertNotNull(completion)
        assertEquals(ChildObservation.STDOUT_EOF, completion!!.firstObservation)
        assertFalse(completion.closing)
        assertNull(completion.stop)
        assertNull(lifecycle.complete(9))
    }

    @Test fun oldChildCompletionKeepsItsOwnAttemptAndGeneration() {
        val old = ChildLifecycle(attemptA, 1L)
        val new = ChildLifecycle(attemptB, 2L)
        new.recordHostStop(ChildStopReason.NETWORK_RECOVERY, "Reconnecting")
        val late = old.complete(137)
        assertNotNull(late)
        assertEquals(attemptA, late!!.attempt)
        assertEquals(1L, late.generation)
        assertEquals(137, late.exitCode)
    }

    @Test fun exitCodeIsReportedVerbatimAndNotTreatedAsSignal() {
        val lifecycle = ChildLifecycle(attemptA, 3L)
        val completion = lifecycle.complete(137)
        assertNotNull(completion)
        assertEquals(137, completion!!.exitCode)
        assertFalse(ChildCompletionDiagnostics.line(completion).contains("signal"))
    }

    @Test fun eofFirstThenHostStopBeforeCompletionKeepsBothFacts() {
        val lifecycle = ChildLifecycle(attemptA, 4L)
        lifecycle.markObservation(ChildObservation.STDOUT_EOF)
        lifecycle.recordHostStop(ChildStopReason.TEARDOWN, "Stopping")
        val completion = lifecycle.complete(0)
        assertNotNull(completion)
        assertEquals(ChildObservation.STDOUT_EOF, completion!!.firstObservation)
        assertTrue(completion.closing)
        assertEquals(ChildStopReason.TEARDOWN, completion.stop?.reason)
        assertEquals("Stopping", completion.stop?.phase)
        assertNull(lifecycle.complete(0))
    }

    @Test fun completionFreezesAndLaterHostStopIsIgnored() {
        val lifecycle = ChildLifecycle(attemptA, 5L)
        lifecycle.markObservation(ChildObservation.STDOUT_EOF)
        val completion = lifecycle.complete(137)
        assertNotNull(completion)
        assertFalse(completion!!.closing)
        assertNull(completion.stop)
        // A host stop after completion must not retroactively change the frozen event.
        lifecycle.recordHostStop(ChildStopReason.TEARDOWN, "Stopping")
        assertNull(lifecycle.complete(137))
        assertEquals(ChildObservation.STDOUT_EOF, completion.firstObservation)
        assertFalse(completion.closing)
        assertNull(completion.stop)
    }

    @Test fun hostStopFirstIsCoherentAndLateObservationIgnored() {
        val lifecycle = ChildLifecycle(attemptA, 6L)
        lifecycle.recordHostStop(ChildStopReason.KILL_SWITCH_HOLD, "KillSwitch")
        lifecycle.markObservation(ChildObservation.STDOUT_EOF)
        val completion = lifecycle.complete(0)
        assertNotNull(completion)
        assertEquals(ChildObservation.HOST_STOP, completion!!.firstObservation)
        assertTrue(completion.closing)
        assertEquals(ChildStopReason.KILL_SWITCH_HOLD, completion.stop?.reason)
    }

    @Test fun hostRecordPublicationIsCoherentUnderContention() {
        for (iteration in 0 until 2000) {
            val lifecycle = ChildLifecycle(attemptA, iteration.toLong())
            val start = CountDownLatch(1)
            val done = CountDownLatch(2)
            val result = AtomicReference<ChildCompletion?>()
            Thread({
                start.await()
                lifecycle.recordHostStop(ChildStopReason.TEARDOWN, "Stopping")
                done.countDown()
            }, "host-stop").start()
            Thread({
                start.await()
                result.set(lifecycle.complete(0))
                done.countDown()
            }, "completion").start()
            start.countDown()
            done.await()
            val completion = result.get() ?: continue
            // Invariants reject torn state regardless of which legal order won.
            if (completion.stop != null) {
                assertTrue("published stop must be coherent with closing", completion.closing)
            }
            if (completion.firstObservation == ChildObservation.HOST_STOP) {
                assertTrue("HOST_STOP observation must be coherent with closing", completion.closing)
            }
            if (!completion.closing) {
                assertNull("closing=false must never hide a published stop", completion.stop)
                assertNotEquals(ChildObservation.HOST_STOP, completion.firstObservation)
            }
        }
    }

    @Test fun diagnosticsLineIsFixedAndSanitized() {
        val lifecycle = ChildLifecycle(attemptA, 8L)
        lifecycle.markObservation(ChildObservation.BRIDGE_INVALID)
        lifecycle.recordHostStop(ChildStopReason.NETWORK_RECOVERY, "Reconnecting")
        val line = ChildCompletionDiagnostics.line(lifecycle.complete(11)!!)
        assertTrue(line.startsWith("boundary=child_exit "))
        assertTrue(line.contains("attempt=$attemptA"))
        assertTrue(line.contains("generation=8"))
        assertTrue(line.contains("exit_code=11"))
        assertTrue(line.contains("first_observation=BRIDGE_INVALID"))
        assertTrue(line.contains("closing=true"))
        assertTrue(line.contains("stop_reason=NETWORK_RECOVERY"))
        assertTrue(line.contains("stop_phase=Reconnecting"))
        assertFalse(line.contains("http"))
        assertFalse(line.contains("://"))
        assertFalse(line.contains("token"))
        assertFalse(line.contains("\n"))
    }

    @Test fun diagnosticsFallsBackOnNonConformingValues() {
        val lifecycle = ChildLifecycle("not-a-uuid", 6L)
        lifecycle.recordHostStop(ChildStopReason.TEARDOWN, "phase with spaces")
        val line = ChildCompletionDiagnostics.line(lifecycle.complete(0)!!)
        assertTrue(line.contains("attempt=-"))
        assertTrue(line.contains("stop_phase=NONE"))
        assertFalse(line.contains("phase with spaces"))
    }

    @Test fun missingObservationRendersFixedNoneTokens() {
        val lifecycle = ChildLifecycle(attemptA, 9L)
        val line = ChildCompletionDiagnostics.line(lifecycle.complete(1)!!)
        assertTrue(line.contains("first_observation=NONE"))
        assertTrue(line.contains("stop_reason=NONE"))
        assertTrue(line.contains("stop_phase=NONE"))
        assertTrue(line.contains("closing=false"))
    }

    @Test fun generationIsMonotonicAcrossChildren() {
        assertTrue(ChildGeneration.next() < ChildGeneration.next())
    }
}

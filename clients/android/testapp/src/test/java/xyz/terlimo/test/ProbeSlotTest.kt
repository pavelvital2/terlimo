package xyz.terlimo.test

import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.RejectedExecutionException
import org.junit.Assert.*
import org.junit.Test

class ProbeSlotTest {
    @Test fun stuckCallDoesNotQueueOrSpawnAnother() {
        val started = CountDownLatch(1)
        val release = CountDownLatch(1)
        val slot = ProbeSlot()
        try {
            slot.submit { started.countDown(); release.await() }
            assertTrue(started.await(2, TimeUnit.SECONDS))
            try { slot.submit { fail("second call ran") }; fail("unbounded admission") }
            catch (_: RejectedExecutionException) { }
        } finally { release.countDown(); slot.close() }
    }
    @Test fun cancellationInterruptsCooperativeCall() {
        val started = CountDownLatch(1)
        val ended = CountDownLatch(1)
        val slot = ProbeSlot()
        try {
            val job = slot.submit { started.countDown(); try { CountDownLatch(1).await() } finally { ended.countDown() } }
            assertTrue(started.await(2, TimeUnit.SECONDS))
            job.cancel(true)
            assertTrue(ended.await(2, TimeUnit.SECONDS))
        } finally { slot.close() }
    }
}

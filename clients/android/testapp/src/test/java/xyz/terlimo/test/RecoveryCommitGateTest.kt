package xyz.terlimo.test

import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class RecoveryCommitGateTest {
    @Test
    fun cancelledSupersededAndForeignInstallationResultsCannotWrite() {
        val gate = RecoveryCommitGate()
        var writes = 0
        val write = { writes++; true }
        gate.begin("old", "installation")
        gate.cancel()
        assertFalse(gate.writeIfCurrent("old", "installation", write))
        gate.begin("new", "installation")
        assertFalse(gate.writeIfCurrent("old", "installation", write))
        assertFalse(gate.writeIfCurrent("new", "other-installation", write))
        assertEquals(0, writes)
        assertTrue(gate.writeIfCurrent("new", "installation", write))
        assertEquals(1, writes)
    }

    @Test
    fun persistenceFailureRemainsAnHonestFailure() {
        val gate = RecoveryCommitGate()
        gate.begin("attempt", "installation")
        assertFalse(gate.writeIfCurrent("attempt", "installation") { false })
    }

    @Test
    fun actualWriteAndCancellationShareTheLock() {
        val gate = RecoveryCommitGate()
        gate.begin("attempt", "installation")
        val writerEntered = CountDownLatch(1)
        val writerMayFinish = CountDownLatch(1)
        val cancelStarted = CountDownLatch(1)
        val cancelled = CountDownLatch(1)
        val executor = Executors.newFixedThreadPool(2)
        try {
            val writer = executor.submit<Boolean> {
                gate.writeIfCurrent("attempt", "installation") {
                    writerEntered.countDown()
                    check(writerMayFinish.await(2, TimeUnit.SECONDS))
                    true
                }
            }
            assertTrue(writerEntered.await(1, TimeUnit.SECONDS))
            val cancellation = executor.submit {
                cancelStarted.countDown()
                gate.cancel()
                cancelled.countDown()
            }
            assertTrue(cancelStarted.await(1, TimeUnit.SECONDS))
            assertFalse(cancelled.await(50, TimeUnit.MILLISECONDS))
            writerMayFinish.countDown()
            assertTrue(writer.get(1, TimeUnit.SECONDS))
            cancellation.get(1, TimeUnit.SECONDS)
            assertFalse(gate.writeIfCurrent("attempt", "installation") { error("late write") })
        } finally {
            writerMayFinish.countDown()
            executor.shutdownNow()
        }
    }
}

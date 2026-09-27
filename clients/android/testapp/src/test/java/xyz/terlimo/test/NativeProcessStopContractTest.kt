package xyz.terlimo.test

import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.InputStream
import java.io.OutputStream
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Focused tests of the real production stop contract used by NativeProcess: a stop is confirmed
 * only by the factual bounded reap (`waitFor == true`), never by the absence of an exception.
 * The managed child implements the same [NativeChild] adapter production wraps.
 */
class NativeProcessStopContractTest {
    private class ManagedChild(
        private val waitResult: Boolean? = true,
        private val waitAction: (() -> Boolean)? = null,
    ) : NativeChild {
        var destroys = 0
        override fun destroyForcibly() { destroys++ }
        override fun waitFor(timeoutMillis: Long): Boolean = waitAction?.invoke() ?: (waitResult ?: false)
        override fun waitForExit(): Int = 0
        override fun exitValue(): Int = 0
        override fun inputStream(): InputStream = ByteArrayInputStream(ByteArray(0))
        override fun errorStream(): InputStream = ByteArrayInputStream(ByteArray(0))
        override fun outputStream(): OutputStream = ByteArrayOutputStream()
    }

    private fun executable(): File = File.createTempFile("terlimo-test", ".bin").apply {
        deleteOnExit()
        setExecutable(true)
    }

    private fun started(child: ManagedChild, spawned: (Int) -> Unit = {}): NativeProcess =
        NativeProcess(executable(), "attempt-stop-contract", {}, {}, {}) { spawned(1); child }
            .also { it.start(JSONObject()) }

    @Test fun factualWaitForTrueConfirmsTheStop() {
        val child = ManagedChild(waitResult = true)
        val process = started(child)
        assertTrue(process.stop(ChildStopReason.KILL_SWITCH_HOLD, "KillSwitch"))
        assertTrue(child.destroys >= 1)
    }

    @Test fun timedOutWaitLeavesTheStopUnconfirmed() {
        val child = ManagedChild(waitResult = false)
        val process = started(child)
        assertFalse(process.stop(ChildStopReason.KILL_SWITCH_HOLD, "KillSwitch"))
    }

    @Test fun noChildIsConfirmedBecauseStartIsClosedOut() {
        val process = NativeProcess(executable(), "attempt-stop-contract", {}, {}, {}) {
            error("a child must never be spawned for this case")
        }
        assertTrue(process.stop(ChildStopReason.KILL_SWITCH_HOLD, "KillSwitch"))
    }

    @Test fun waitExceptionLeavesTheStopUnconfirmed() {
        val child = ManagedChild(waitAction = { throw IllegalStateException("reap failed") })
        val process = started(child)
        assertFalse(process.stop(ChildStopReason.KILL_SWITCH_HOLD, "KillSwitch"))
    }

    @Test fun interruptionIsRestoredAndLeavesTheStopUnconfirmed() {
        val child = ManagedChild(waitAction = { throw InterruptedException("interrupted") })
        val process = started(child)
        assertFalse(process.stop(ChildStopReason.KILL_SWITCH_HOLD, "KillSwitch"))
        assertTrue(Thread.interrupted()) // restored by the stop contract
        assertFalse(Thread.interrupted()) // and cleared again for the test thread
    }

    @Test fun startAfterUnconfirmedStopCanNeverSpawnAnotherChild() {
        var spawns = 0
        val child = ManagedChild(waitResult = false)
        val process = NativeProcess(executable(), "attempt-stop-contract", {}, {}, {}) { spawns++; child }
        process.start(JSONObject())
        assertEquals(1, spawns)
        assertFalse(process.stop(ChildStopReason.KILL_SWITCH_HOLD, "KillSwitch"))
        process.start(JSONObject())
        assertEquals(1, spawns)
    }
}

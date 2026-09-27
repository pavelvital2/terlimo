package xyz.terlimo.test

import java.nio.file.Files
import java.nio.file.Paths
import org.junit.Assert.*
import org.junit.Test

class ChildLifecycleSourceTest {
    private fun source(path: String) = String(Files.readAllBytes(Paths.get(path)), Charsets.UTF_8)

    @Test fun everyHostStopUsesAFixedReasonEnumAndPhase() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        listOf("TEARDOWN", "SLEEP_PAUSE", "NETWORK_RECOVERY", "RECOVERY_RETRY", "KILL_SWITCH_HOLD").forEach {
            assertTrue("missing ChildStopReason.$it", service.contains("ChildStopReason.$it"))
        }
        assertFalse(service.contains("child?.stop()"))
        assertEquals(5, Regex("child\\?\\.stop\\(ChildStopReason\\.").findAll(service).count())
    }

    @Test fun stateIsPublishedThroughSingleCoherentAtomicReference() {
        val lifecycle = source("src/main/java/xyz/terlimo/test/ChildLifecycle.kt")
        assertTrue(lifecycle.contains("AtomicReference(ChildState("))
        assertTrue(lifecycle.contains("state.compareAndSet(current, next)"))
        assertTrue(lifecycle.contains("val next = current.copy("))
        // No split atomics: a single reference publishes firstObservation + stop + frozen together.
        assertFalse("split captured boolean must not exist", lifecycle.contains("AtomicBoolean"))
        assertFalse("separate stop reference must not exist", lifecycle.contains("AtomicReference<ChildStopRecord"))
        assertFalse(lifecycle.contains("stop.set("))
    }

    @Test fun singleExitWatcherIsSoleCompletionSiteAndUsesRealReap() {
        val native = source("src/main/java/xyz/terlimo/test/NativeProcess.kt")
        assertTrue(native.contains("private fun startExitWatcher(child: NativeChild)"))
        assertTrue(native.contains("startExitWatcher(child)"))
        val watcher = native.substringAfter("private fun startExitWatcher(child: NativeChild)")
            .substringBefore("companion object")
        assertTrue(watcher.contains("child.waitForExit()"))
        assertTrue(watcher.contains("child.exitValue()"))
        assertTrue(watcher.contains("lifecycle.complete(exitCode)?.let(onCompletion)"))
        assertEquals(1, Regex("onCompletion\\)").findAll(native).count())
        assertFalse(native.contains("EXIT_UNAVAILABLE"))
        assertFalse(native.contains("finishChild"))
    }

    @Test fun readerMarksBridgeObservationWithoutWaitingForExit() {
        val native = source("src/main/java/xyz/terlimo/test/NativeProcess.kt")
        val reader = native.substringAfter("reader.execute {")
            .substringBefore("if (closing) { child.destroyForcibly(); return }")
        val observationAt = reader.indexOf("lifecycle.markObservation(ChildObservation.STDOUT_EOF)")
        val callbackAt = reader.indexOf("failureCode?.let(onFailure)")
        assertTrue("observation must precede terminal callback", observationAt in 1 until callbackAt)
        assertTrue(reader.contains("ChildObservation.BRIDGE_INVALID"))
        assertFalse("reader must not wait for process exit", reader.contains("waitFor"))
        assertFalse(reader.contains("onCompletion"))
        assertFalse(reader.contains("markUnexpected"))
        assertFalse(reader.contains("if (!closing) onFailure("))
    }

    @Test fun stderrStillDiscardedAndCompletionHasNoArbitraryText() {
        val diagnostics = source("src/main/java/xyz/terlimo/test/NativeDiagnostics.kt")
        assertTrue(diagnostics.contains("discard"))
        assertFalse(diagnostics.contains("Log."))
        val lifecycle = source("src/main/java/xyz/terlimo/test/ChildLifecycle.kt")
        assertTrue(lifecycle.contains("enum class ChildStopReason"))
        assertTrue(lifecycle.contains("enum class ChildObservation"))
        assertFalse(lifecycle.contains("throwable"))
        assertFalse(lifecycle.contains("message"))
        assertFalse(lifecycle.contains("Log."))
        assertFalse(lifecycle.contains("println"))
    }

    @Test fun serviceLogsCompletionViaFixedFormatterOnly() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(service.contains("ChildCompletionDiagnostics.line(completion)"))
        assertFalse(service.contains("completion.toString()"))
    }

    @Test fun teardownRecoveryAdmissionLifetimeBehaviourUnchanged() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(service.contains("TerminalFailurePolicy.outcome("))
        assertTrue(service.contains("PhysicalNetworkRecovery.retry("))
        assertTrue(service.contains("leaseAlarms.close()"))
        assertTrue(service.contains("gate.cancel()"))
    }
}

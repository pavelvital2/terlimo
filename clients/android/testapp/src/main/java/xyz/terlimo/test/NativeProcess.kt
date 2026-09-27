package xyz.terlimo.test

import org.json.JSONObject
import java.io.BufferedOutputStream
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.InputStream
import java.io.OutputStream
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit

/**
 * Minimal managed-child seam. Production wraps [java.lang.Process]; focused tests drive the real
 * stop contract with a managed fake without changing the production lifecycle.
 */
internal interface NativeChild {
    fun destroyForcibly()
    fun waitFor(timeoutMillis: Long): Boolean
    /** Blocking reap used by the sole exit watcher. */
    fun waitForExit(): Int
    fun exitValue(): Int
    fun inputStream(): InputStream
    fun errorStream(): InputStream
    fun outputStream(): OutputStream
}

internal class ProcessNativeChild(private val process: Process) : NativeChild {
    override fun destroyForcibly() { process.destroyForcibly() }
    override fun waitFor(timeoutMillis: Long): Boolean = process.waitFor(timeoutMillis, TimeUnit.MILLISECONDS)
    override fun waitForExit(): Int = process.waitFor()
    override fun exitValue(): Int = process.exitValue()
    override fun inputStream(): InputStream = process.inputStream
    override fun errorStream(): InputStream = process.errorStream
    override fun outputStream(): OutputStream = process.outputStream
}

/** Only anonymous process pipes carry secrets; argv, environment and stderr never do. */
internal class NativeProcess(
    private val executable: File,
    private val attempt: String,
    private val onMessage: (JSONObject) -> Unit,
    private val onFailure: (String) -> Unit,
    private val onCompletion: (ChildCompletion) -> Unit,
    /** Mirrored, already-allowlisted fixed stderr code; display/diagnostic only. */
    private val onStderrCode: (String) -> Unit = {},
    private val spawn: (String) -> NativeChild =
        { path -> ProcessNativeChild(ProcessBuilder(path, "--android-bridge").start()) }
) {
    @Volatile private var process: NativeChild? = null
    private var writer: BufferedOutputStream? = null
    private val reader = Executors.newSingleThreadExecutor()
    @Volatile private var closing = false
    /** Correlation and exactly-once completion for this concrete child only. */
    private val lifecycle = ChildLifecycle(attempt, ChildGeneration.next())

    @Synchronized fun start(message: JSONObject) {
        if (closing) return
        check(process == null)
        check(executable.isFile && executable.canExecute()) { "NATIVE_UNAVAILABLE" }
        val child = spawn(executable.absolutePath)
        process = child
        startExitWatcher(child)
        if (closing) { child.destroyForcibly(); return }
        writer = BufferedOutputStream(child.outputStream())
        // Native diagnostics may contain transport details: mirror only the fixed,
        // secret-free accountaccess codes (historical bare format) and source-qualified
        // onboarding tokens, and discard everything else unlogged.
        Thread({ drainNativeStderr(child.errorStream()) { code ->
            android.util.Log.w(NativeStderrCodes.TAG, "code=$code")
            runCatching { onStderrCode(code) }
        } }, "native-stderr").start()
        reader.execute {
            var failureCode: String? = null
            try {
                child.inputStream().use { input ->
                    val line = ByteArrayOutputStream()
                    while (!closing) {
                        val byte = input.read()
                        if (byte < 0) break
                        if (byte == 10) {
                            check(line.size() > 0) { "BRIDGE_EMPTY" }
                            val event = JSONObject(line.toString("UTF-8"))
                            line.reset()
                            check(event.getInt("v") == 1 && event.getString("attempt_id") == attempt) { "BRIDGE_CORRELATION" }
                            onMessage(event)
                        } else {
                            check(line.size() < MAX_LINE) { "BRIDGE_TOO_LARGE" }
                            line.write(byte)
                        }
                    }
                }
                if (!closing) { lifecycle.markObservation(ChildObservation.STDOUT_EOF); failureCode = "NATIVE_EXIT" }
            } catch (_: Exception) {
                if (!closing) { lifecycle.markObservation(ChildObservation.BRIDGE_INVALID); failureCode = "BRIDGE_INVALID" }
            }
            // stdout EOF/bridge failure is NOT process exit: record the causal observation (so a
            // synchronous terminal teardown cannot re-label it) and hand off immediately. The
            // dedicated exit watcher, not this reader, emits the single real completion.
            failureCode?.let(onFailure)
        }
        if (closing) { child.destroyForcibly(); return }
        send(message)
    }
    @Synchronized fun send(message: JSONObject) {
        if (closing) return
        message.put("v", 1).put("attempt_id", attempt)
        val bytes = message.toString().toByteArray()
        require(bytes.size <= MAX_LINE) { "BRIDGE_TOO_LARGE" }
        val stream = writer ?: error("BRIDGE_CLOSED")
        stream.write(bytes); stream.write(10); stream.flush()
    }
    /**
     * Truthful write: `true` only after a real write+flush. Never throws and never changes the
     * behavior of [send] used by every other caller. Used by switch diagnostics and by the
     * single-flight purchase send, whose holder must not be left busy by an escaped exception.
     */
    @Synchronized fun trySend(message: JSONObject): Boolean {
        if (closing) return false
        message.put("v", 1).put("attempt_id", attempt)
        val bytes = message.toString().toByteArray()
        if (bytes.size > MAX_LINE) return false
        val stream = writer ?: return false
        return try {
            stream.write(bytes); stream.write(10); stream.flush()
            true
        } catch (_: Exception) {
            false
        }
    }
    /**
     * Caller invalidates attempt first, so no late signature/state can be accepted.
     *
     * The fixed [reason] + [phase] are recorded before the child is destroyed, so the single
     * completion event can distinguish a host stop from an unexpected native exit.
     *
     * Returns the factual stop result: `true` only when the child was actually reaped within the
     * existing bounded window, or when no child was attached after the synchronized re-read while
     * closing (start() cannot spawn one any more). A timeout, exception or interruption is an
     * unconfirmed stop; the interruption is restored. The sole exit watcher remains the only
     * source of completion diagnostics and lifecycle.complete is never duplicated here.
     */
    fun stop(reason: ChildStopReason, phase: String): Boolean {
        lifecycle.recordHostStop(reason, phase)
        // Do not wait for send's monitor or try writing cancel into a full pipe.
        // Invalidation happened in the host; process death/EOF releases native waiters.
        closing = true
        var child = process
        child?.destroyForcibly()
        synchronized(this) {
            child = process
            child?.destroyForcibly()
            runCatching { writer?.close() }
            writer = null
        }
        var confirmed = false
        try {
            confirmed = if (child == null) {
                // start() is closed out and cannot attach a child any more.
                synchronized(this) { process == null }
            } else {
                child.waitFor(EXIT_WAIT_MS)
            }
        } catch (_: InterruptedException) {
            Thread.currentThread().interrupt()
            confirmed = false
        } catch (_: Exception) {
            confirmed = false
        } finally {
            reader.shutdownNow()
        }
        return confirmed
    }

    /**
     * Sole completion site for this concrete child. Starts once, immediately after the child is
     * created, and does a real blocking reap off main/UI/reader/teardown. Only after the OS
     * actually reaps the process does it read the exit code and emit exactly one event.
     */
    private fun startExitWatcher(child: NativeChild) {
        Thread({
            val exitCode = try {
                child.waitForExit()
                child.exitValue()
            } catch (_: InterruptedException) {
                Thread.currentThread().interrupt()
                return@Thread
            }
            lifecycle.complete(exitCode)?.let(onCompletion)
        }, "native-exit-watcher").apply { isDaemon = true }.start()
    }

    companion object {
        const val MAX_LINE = 262_144
        const val EXIT_WAIT_MS = 1500L
    }
}

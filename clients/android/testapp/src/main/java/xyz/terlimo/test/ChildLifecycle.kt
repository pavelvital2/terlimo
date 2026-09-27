package xyz.terlimo.test

import java.util.concurrent.atomic.AtomicLong
import java.util.concurrent.atomic.AtomicReference

/**
 * Fixed, secret-free reasons why the host intentionally stops a native child.
 *
 * Deliberately an enum (never a free-form string) so a stop reason can never carry
 * exception text, URLs, payloads, headers or keys into diagnostics.
 */
internal enum class ChildStopReason {
    TEARDOWN,
    SLEEP_PAUSE,
    NETWORK_RECOVERY,
    RECOVERY_RETRY,
    KILL_SWITCH_HOLD,
}

/**
 * Fixed, secret-free causal observations of a single native child.
 *
 * `STDOUT_EOF` and `BRIDGE_INVALID` are host-side bridge observations only; they are NOT proof
 * of a spontaneous process death, OS kill or signal, and must never be read that way.
 */
internal enum class ChildObservation {
    NONE,
    HOST_STOP,
    STDOUT_EOF,
    BRIDGE_INVALID,
}

/** Host-side fixed phase + reason recorded before a host-initiated stop. */
internal data class ChildStopRecord(val reason: ChildStopReason, val phase: String)

/** Immutable per-child state so transitions publish coherently through one reference. */
internal data class ChildState(
    val firstObservation: ChildObservation,
    val stop: ChildStopRecord?,
    val frozen: Boolean,
)

/** Fixed, sanitized payload of the single completion event for one Process child. */
internal data class ChildCompletion(
    val attempt: String,
    val generation: Long,
    val exitCode: Int,
    val firstObservation: ChildObservation,
    val closing: Boolean,
    val stop: ChildStopRecord?,
)

/**
 * Exactly-once completion bookkeeping for one native Process child.
 *
 * Pure Kotlin (no Android API) so the accounting is directly unit-testable. `attempt` and
 * `generation` belong to this child only, so a late completion can never be attributed to a
 * newer attempt. `exitCode` is the raw process exit status; it is reported verbatim and is
 * never interpreted as a signal.
 *
 * All transitions are CAS replacements of one immutable [ChildState]. A host stop therefore
 * publishes `firstObservation` and `stop` together, so the exit watcher can never snapshot a
 * torn state (e.g. `closing=false` while a pre-completion host stop was already published).
 * The first observation wins; a host stop that happens after a bridge observation but before
 * completion is retained as `stop` while `firstObservation` stays the bridge observation.
 * Completion freezes the state, after which later writes are ignored.
 */
internal class ChildLifecycle(private val attempt: String, private val generation: Long) {
    private val state = AtomicReference(ChildState(ChildObservation.NONE, null, frozen = false))

    /** First action of a host stop, before `closing`/destroy: fixed reason + phase. */
    fun recordHostStop(reason: ChildStopReason, phase: String) {
        val record = ChildStopRecord(reason, phase)
        while (true) {
            val current = state.get()
            if (current.frozen) return
            val first = if (current.firstObservation == ChildObservation.NONE) {
                ChildObservation.HOST_STOP
            } else {
                current.firstObservation
            }
            val next = current.copy(firstObservation = first, stop = current.stop ?: record)
            if (state.compareAndSet(current, next)) return
        }
    }

    /** Reader bridge observation (stdout EOF / bridge failure); first observation wins. */
    fun markObservation(observation: ChildObservation) {
        while (true) {
            val current = state.get()
            if (current.frozen || current.firstObservation != ChildObservation.NONE) return
            val next = current.copy(firstObservation = observation)
            if (state.compareAndSet(current, next)) return
        }
    }

    /** Returns this child's single completion (freezing the state), or null if already produced. */
    fun complete(exitCode: Int): ChildCompletion? {
        while (true) {
            val current = state.get()
            if (current.frozen) return null
            val next = current.copy(frozen = true)
            if (state.compareAndSet(current, next)) {
                return ChildCompletion(
                    attempt = attempt,
                    generation = generation,
                    exitCode = exitCode,
                    firstObservation = next.firstObservation,
                    closing = next.stop != null,
                    stop = next.stop,
                )
            }
        }
    }
}

/** Monotonic per-process child generation; fixed correlation, never user controlled. */
internal object ChildGeneration {
    private val counter = AtomicLong(0)
    fun next(): Long = counter.incrementAndGet()
}

/** Renders the completion with a fixed shape and validated values only. */
internal object ChildCompletionDiagnostics {
    const val TAG = "WDTT/ChildExit"
    private val PHASE = Regex("[A-Za-z][A-Za-z0-9_]{0,31}")
    private val ATTEMPT = Regex("[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")

    fun line(completion: ChildCompletion): String {
        val attempt = if (ATTEMPT.matches(completion.attempt)) completion.attempt else "-"
        val reason = completion.stop?.reason?.name ?: "NONE"
        val phase = completion.stop?.takeIf { PHASE.matches(it.phase) }?.phase ?: "NONE"
        return "boundary=child_exit attempt=$attempt generation=${completion.generation} " +
            "exit_code=${completion.exitCode} first_observation=${completion.firstObservation.name} " +
            "closing=${completion.closing} stop_reason=$reason stop_phase=$phase"
    }
}

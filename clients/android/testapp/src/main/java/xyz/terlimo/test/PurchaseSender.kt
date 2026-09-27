package xyz.terlimo.test

import org.json.JSONObject

/** The single-flight purchase send outcome; each maps to exactly one host action. */
internal enum class PurchaseSendOutcome {
    /** Another purchase request still occupies the stream: nothing was sent or rotated. */
    WAITING,
    /** Local preparation failed after capture: nothing reached the wire, the capture is freed. */
    LOCAL_FAILURE,
    /** The frame was really written and flushed: the request is now outstanding. */
    WRITTEN,
    /** The write result is unknown (exception/closed/partial): the attempt must be fenced. */
    UNKNOWN_WRITE,
}

/**
 * One purchase send under the host-owned single-flight [PurchaseSingleFlight].
 *
 * The previous path called the throwing [NativeProcess.send] from inside a BridgeActor task:
 * a `BRIDGE_CLOSED`/`IOException` would escape into the executor (which swallows it), so the
 * single-flight holder stayed busy forever and `sending` was never published. This makes the
 * write result truthful and total:
 *
 * - it captures the stream before any durable key is touched;
 * - a local preparation error frees the capture immediately (nothing was sent);
 * - it writes with the truthful, non-throwing [NativeProcess.trySend];
 * - an unknown/closed write keeps the capture and returns [PurchaseSendOutcome.UNKNOWN_WRITE],
 *   so the caller performs the standard terminal transport teardown (whose epoch fence drops
 *   the capture). The holder is never released directly on a possibly partial write.
 */
internal class PurchaseSender(private val flight: PurchaseSingleFlight) {
    fun send(
        request: PurchaseFlight,
        prepare: () -> JSONObject,
        write: (JSONObject) -> Boolean,
    ): PurchaseSendOutcome {
        if (!flight.acquire(request)) return PurchaseSendOutcome.WAITING
        val message = try {
            prepare()
        } catch (_: Throwable) {
            // Nothing reached the wire: no partial write risk, free the capture at once.
            flight.release(request)
            return PurchaseSendOutcome.LOCAL_FAILURE
        }
        val written = try {
            write(message)
        } catch (_: Throwable) {
            false
        }
        return if (written) PurchaseSendOutcome.WRITTEN else PurchaseSendOutcome.UNKNOWN_WRITE
    }
}

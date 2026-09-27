package xyz.terlimo.test

/** A single user Connect owns one budget, including queued work and readiness. */
internal class ConnectDeadline {
    private var attempt: String? = null
    private var end = Long.MAX_VALUE

    @Synchronized fun start(id: String, now: Long): Boolean {
        if (attempt != null) return false
        attempt = id
        end = now + MILLIS
        return true
    }
    @Synchronized fun deadline(id: String): Long = if (attempt == id) end else Long.MAX_VALUE
    @Synchronized fun expired(id: String, now: Long): Boolean = attempt == id && now >= end
    @Synchronized fun complete(id: String, now: Long): Boolean {
        if (attempt != id) return true // Recovery uses its existing outer budget.
        if (now >= end) return false
        clear()
        return true
    }
    @Synchronized fun clear() { attempt = null; end = Long.MAX_VALUE }

    companion object { const val MILLIS = 15_000L }
}

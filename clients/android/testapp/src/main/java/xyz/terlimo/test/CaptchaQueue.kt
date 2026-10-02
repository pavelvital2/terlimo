package xyz.terlimo.test

/**
 * Single-owner CAPTCHA request queue. Every mutation runs on the SessionService actor;
 * this class is deliberately pure so exactly-once, stale and cancellation policy can be
 * proven on the JVM. A late completion of an old request can never clear or overwrite a
 * newer prompt or a newer attempt.
 */
internal class CaptchaQueue {
    enum class Decision { IGNORE_DUPLICATE, BUSY, ACCEPT }
    data class Entry(val attempt: String, val id: String, val mode: String,
        val url: String, val sessionToken: String, val catalogOwner: CatalogCaptchaOwner? = null)

    private var active: Entry? = null
    private var lastCompletedId: String? = null

    @Synchronized fun request(attempt: String, id: String): Decision {
        val current = active
        if (current != null) {
            return if (current.attempt == attempt && current.id == id) Decision.IGNORE_DUPLICATE else Decision.BUSY
        }
        if (lastCompletedId == id) return Decision.IGNORE_DUPLICATE
        return Decision.ACCEPT
    }

    @Synchronized fun activate(entry: Entry) { active = entry }

    /** Completes exactly once; returns the entry only for the exact active identity. */
    @Synchronized fun complete(attempt: String, id: String): Entry? {
        val current = active ?: return null
        if (current.attempt != attempt || current.id != id) return null
        active = null
        lastCompletedId = id
        return current
    }

    /** Disconnect/teardown: the active prompt dies, later callbacks are dropped. */
    @Synchronized fun invalidate() { active = null }

    @Synchronized fun activeEntry(): Entry? = active
}

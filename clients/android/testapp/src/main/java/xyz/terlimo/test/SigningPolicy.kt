package xyz.terlimo.test

import java.util.Base64

/** Pure boundary checks; signing always receives the transcript, never its digest. */
internal object SigningPolicy {
    fun validateWorker(kind: String, worker: String) {
        require(
            (kind == "bootstrap" && worker == "bootstrap") ||
                (kind == "vpn" && worker.matches(Regex("0|[1-9][0-9]{0,8}")))
        ) { "SIGN_WORKER_INVALID" }
    }
    fun validate(kind: String, transcript: ByteArray) {
        val domain = when (kind) {
            "bootstrap" -> "WLBS-POP-1\u0000".toByteArray(Charsets.US_ASCII)
            "vpn" -> "WL-VPN-POP-1\u0000".toByteArray(Charsets.US_ASCII)
            else -> error("SIGN_KIND_INVALID")
        }
        val fields = if (kind == "bootstrap") 16 + 16 + 32 + 32 else 32 + 16 + 32 + 32
        require(transcript.size == domain.size + fields && transcript.take(domain.size).toByteArray().contentEquals(domain)) {
            "SIGN_TRANSCRIPT_INVALID"
        }
    }
    fun decode(value: String): ByteArray {
        require(value.matches(Regex("[A-Za-z0-9_-]*"))) { "BASE64_INVALID" }
        val bytes = Base64.getUrlDecoder().decode(value)
        require(encode(bytes) == value) { "BASE64_NONCANONICAL" }
        return bytes
    }
    fun encode(value: ByteArray): String = Base64.getUrlEncoder().withoutPadding().encodeToString(value)
}

internal class AttemptGate {
    @Volatile var active: String? = null
        private set
    private val pending = mutableSetOf<String>()
    @Synchronized fun start(id: String) { check(active == null); active = id; pending.clear() }
    @Synchronized fun admit(attempt: String, id: String, deadline: Long, now: Long): Boolean {
        if (attempt != active || id.isBlank() || deadline <= now || deadline - now > 60_000 || pending.size >= 4) return false
        return pending.add(id)
    }
    @Synchronized fun finish(attempt: String, id: String): Boolean = attempt == active && pending.remove(id)
    @Synchronized fun ifActive(attempt: String, action: () -> Unit): Boolean {
        if (attempt != active) return false
        action()
        return true
    }
    @Synchronized fun cancel() { active = null; pending.clear() }
}

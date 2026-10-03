package xyz.terlimo.test

/** Main-thread owned scheduling/decision state, independent of Android and transport. */
internal class AppUpdateFlow {
    data class Check(val manual: Boolean)
    private val queue = java.util.ArrayDeque<Check>()
    private val connections = LinkedHashSet<String>()
    var checking = false; private set
    var dismissedVersion = 0L
    var notifiedVersion = 0L
    var stage = "idle"
    var targetVersion = 0L

    fun connection(id: String) {
        if (id.isEmpty() || !connections.add(id)) return
        if (connections.size > 256) connections.remove(connections.first())
        queue.add(Check(false))
    }
    fun manual() { if (queue.none { it.manual }) queue.add(Check(true)) }
    fun next(): Check? {
        if (checking || queue.isEmpty()) return null
        checking = true
        return queue.removeFirst()
    }
    fun finished() { checking = false }
    fun shouldOffer(version: Long, manual: Boolean) = manual ||
        (version != dismissedVersion && version != notifiedVersion)
    fun later() { dismissedVersion = targetVersion }
    fun reconcile(installed: Long, fileExists: Boolean): String {
        stage = when {
            targetVersion > 0 && installed >= targetVersion -> "installed"
            stage == "downloading" -> "retry"
            stage in setOf("ready", "permission", "installing") && !fileExists -> "retry"
            else -> stage
        }
        return stage
    }
    fun permissionReturned(granted: Boolean): Boolean {
        if (stage != "permission") return false
        stage = "ready"
        return granted
    }
    fun cancelled() { stage = "retry" }
}

/** Exactly one accepted service-success event per user-owned native attempt. */
internal class AppUpdateConnectionGate {
    private var attempt: String? = null
    private var emitted = false
    fun begin(id: String, userOwned: Boolean) { attempt = if (userOwned) id else null; emitted = false }
    fun accepted(id: String): Boolean {
        if (attempt != id || emitted) return false
        emitted = true
        return true
    }
}

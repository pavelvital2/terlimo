package xyz.terlimo.test

/** Actual service-channel boundaries, never timer-generated progress. */
internal enum class CatalogStage(val wire: String, val label: String, val budgetMs: Long) {
    CONNECTING("connecting_server", "Подключение к серверу", 20_000),
    DEVICE("checking_device", "Проверка устройства", 25_000),
    SUBSCRIPTION("subscription_status", "Статус подписки", 10_000),
    LIST("loading_catalog", "Получение списка", 10_000);
    companion object { fun parse(wire: String) = entries.firstOrNull { it.wire == wire } }
}

/** One finite operation in the existing owner; repeated/stale progress cannot extend it. */
internal class CatalogStages {
    var attempt: String? = null; private set
    var cycle: String? = null; private set
    var stage: CatalogStage? = null; private set
    private var totalEnd = 0L
    private var enteredAt = 0L
    private val spent = mutableMapOf<CatalogStage, Long>()
    fun begin(attempt: String, cycle: String, now: Long) {
        this.attempt = attempt; this.cycle = cycle; stage = CatalogStage.CONNECTING
        totalEnd = now + TOTAL_MS
        enteredAt = now
        spent.clear()
    }
    fun advance(attempt: String, cycle: String, next: CatalogStage, now: Long): Boolean {
        val current = stage ?: return false
        if (this.attempt != attempt || this.cycle != cycle || next == current || now >= end()) return false
        spent[current] = (spent[current] ?: 0L) + (now - enteredAt).coerceAtLeast(0)
        enteredAt = now
        stage = next
        return true
    }
    fun remaining(now: Long) = (end() - now).coerceAtLeast(0)
    fun canAccept(attempt: String, cycle: String, now: Long): Boolean =
        stage != null && this.attempt == attempt && this.cycle == cycle && now < end()
    // A real reconnect resumes only the unused active budget; elapsed time in AUTH
    // cannot consume connection time. The whole-operation wall cap never pauses.
    private fun end(): Long {
        val current = stage ?: return totalEnd
        return minOf(totalEnd, enteredAt + current.budgetMs - (spent[current] ?: 0L))
    }
    fun clear() { attempt = null; cycle = null; stage = null; spent.clear() }
    companion object { const val TOTAL_MS = 65_000L }
}

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
    private var furthest = CatalogStage.CONNECTING
    private val stageEnds = mutableMapOf<CatalogStage, Long>()
    fun begin(attempt: String, cycle: String, now: Long) {
        this.attempt = attempt; this.cycle = cycle; stage = CatalogStage.CONNECTING
        totalEnd = now + TOTAL_MS
        furthest = CatalogStage.CONNECTING
        stageEnds.clear(); stageEnds[CatalogStage.CONNECTING] = now + CatalogStage.CONNECTING.budgetMs
    }
    fun advance(attempt: String, cycle: String, next: CatalogStage, now: Long): Boolean {
        val current = stage ?: return false
        if (this.attempt != attempt || this.cycle != cycle || next == current || now >= end()) return false
        stageEnds.putIfAbsent(next, now + next.budgetMs)
        stage = next
        if (next.ordinal > furthest.ordinal) furthest = next
        return true
    }
    fun remaining(now: Long) = (end() - now).coerceAtLeast(0)
    fun canAccept(attempt: String, cycle: String, now: Long): Boolean =
        stage != null && this.attempt == attempt && this.cycle == cycle && now < end()
    private fun end() = minOf(totalEnd, stageEnds[stage] ?: totalEnd, stageEnds[furthest] ?: totalEnd)
    fun clear() { attempt = null; cycle = null; stage = null; stageEnds.clear() }
    companion object { const val TOTAL_MS = 65_000L }
}

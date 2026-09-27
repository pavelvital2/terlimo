package xyz.terlimo.test

/** Secret-free immutable document. The Android adapter must persist it atomically after validation. */
internal data class RoutingSettingsDocument(
    val schemaVersion: Int = 1,
    val revision: Long,
    val routing: RoutingPolicy,
) {
    init {
        require(schemaVersion == 1 && revision >= 0) { "SETTINGS_VERSION_INVALID" }
    }

    fun safeSummary(): String = listOf(
        "schema=$schemaVersion",
        "revision=$revision",
        "app_mode=${routing.apps.mode}",
        "app_rules=${routing.apps.packageNames.size}",
        "dns_mode=vpn_config",
    ).joinToString(" ")
}

internal interface AtomicRoutingSettingsStore {
    /** Compare-and-set prevents concurrent editors and retains last-good state on write failure. */
    fun compareAndSet(expectedRevision: Long?, next: RoutingSettingsDocument): Boolean
    fun read(): RoutingSettingsDocument?
}

/** Deterministic, bounded, secret-free payload for an encrypted atomic Android store. */
internal object RoutingSettingsCodec {
    private const val MAX_BYTES = 64 * 1024

    fun encode(value: RoutingSettingsDocument): String = buildList {
        add("version\t${value.schemaVersion}")
        add("revision\t${value.revision}")
        add("app_mode\t${value.routing.apps.mode.name}")
        value.routing.apps.packageNames.sorted().forEach { add("app\t$it") }
        value.routing.apps.quickExclusionIds.sorted().forEach { add("quick\t$it") }
        add("dns\tautomatic")
    }.joinToString("\n", postfix = "\n").also {
        require(it.toByteArray(Charsets.UTF_8).size <= MAX_BYTES) { "SETTINGS_OVERSIZED" }
    }

    fun decode(
        raw: String,
        protectedPackages: Set<String>,
        presets: QuickExclusionCatalog,
    ): RoutingSettingsDocument {
        require(raw.toByteArray(Charsets.UTF_8).size in 1..MAX_BYTES && raw.endsWith('\n')) { "SETTINGS_INVALID" }
        val rows = raw.dropLast(1).lines().map { it.split('\t') }
        require(rows.all { it.size in 2..3 && it.all(String::isNotEmpty) }) { "SETTINGS_INVALID" }
        fun one(key: String): String = rows.singleOrNull { it[0] == key && it.size == 2 }?.get(1)
            ?: throw IllegalArgumentException("SETTINGS_INVALID")
        val version = one("version").toIntOrNull() ?: throw IllegalArgumentException("SETTINGS_INVALID")
        val revision = one("revision").toLongOrNull() ?: throw IllegalArgumentException("SETTINGS_INVALID")
        val appMode = runCatching { AppRoutingMode.valueOf(one("app_mode")) }.getOrElse { throw IllegalArgumentException("SETTINGS_INVALID") }
        val appRows = rows.filter { it[0] == "app" && it.size == 2 }.map { it[1] }
        val quickRows = rows.filter { it[0] == "quick" && it.size == 2 }.map { it[1] }
        val knownKeys = setOf("version", "revision", "app_mode", "app", "quick", "dns")
        require(rows.all { it[0] in knownKeys }) { "SETTINGS_UNKNOWN_FIELD" }
        require(one("dns") == "automatic") { "SETTINGS_DNS_INVALID" }
        return RoutingSettingsDocument(
            version,
            revision,
            RoutingPolicy(RoutingPolicyNormalizer.apps(appMode, appRows, quickRows, protectedPackages, presets)),
        )
    }
}

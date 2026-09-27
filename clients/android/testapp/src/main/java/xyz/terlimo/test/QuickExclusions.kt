package xyz.terlimo.test

internal data class QuickExclusionPreset(
    val id: String,
    val version: Int,
    val packageNames: Set<String>,
)

internal class QuickExclusionCatalog(presets: Collection<QuickExclusionPreset>) {
    private val byId = presets.associateBy { it.id }.also {
        require(it.size == presets.size && presets.size <= 32) { "QUICK_PRESET_INVALID" }
    }

    init {
        presets.forEach { preset ->
            require(preset.id.matches(Regex("[a-z0-9_-]{1,40}")) && preset.version > 0 && preset.packageNames.isNotEmpty()) {
                "QUICK_PRESET_INVALID"
            }
            preset.packageNames.forEach(RoutingPolicyNormalizer::packageName)
        }
    }

    fun expand(ids: Set<String>): Set<String> = ids.flatMap { id ->
        byId[id]?.packageNames ?: throw IllegalArgumentException("QUICK_PRESET_UNKNOWN")
    }.toSortedSet()
}

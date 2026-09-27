package xyz.terlimo.test

internal enum class AppRoutingMode { DISABLED, EXCLUDE, INCLUDE_ONLY }

internal data class AppRoutingPolicy(
    val mode: AppRoutingMode,
    val packageNames: Set<String>,
    val quickExclusionIds: Set<String> = emptySet(),
)

internal data class RoutingPolicy(val apps: AppRoutingPolicy)

internal object RoutingPolicyNormalizer {
    private val packagePattern = Regex("[A-Za-z][A-Za-z0-9_]*(\\.[A-Za-z0-9_]+)+")

    fun apps(
        mode: AppRoutingMode,
        packages: Collection<String>,
        quickExclusionIds: Collection<String>,
        protectedPackages: Set<String>,
        presets: QuickExclusionCatalog,
    ): AppRoutingPolicy {
        require(packages.size <= 256) { "APP_RULE_LIMIT" }
        val normalizedProtected = protectedPackages.map(::packageName).toSet()
        val packageList = packages.map(::packageName)
        require(packageList.distinct().size == packageList.size) { "APP_RULE_DUPLICATE" }
        // Semantic user selection never contains protected/self packages. A stale legacy
        // self entry is stripped on read so it cannot block or distort user choice.
        val normalized = packageList.filterNot { it in normalizedProtected }.toMutableSet()
        val quickList = quickExclusionIds.map { it.trim() }
        require(quickList.distinct().size == quickList.size) { "QUICK_PRESET_DUPLICATE" }
        val quick = quickList.toSet()
        val expanded = presets.expand(quick)
        when (mode) {
            AppRoutingMode.DISABLED -> require(normalized.isEmpty() && quick.isEmpty()) { "APP_RULE_DISABLED_CONFLICT" }
            AppRoutingMode.EXCLUDE -> {
                normalized += expanded
                normalized -= normalizedProtected
            }
            AppRoutingMode.INCLUDE_ONLY -> require(quick.isEmpty()) { "QUICK_EXCLUSION_MODE_CONFLICT" }
        }
        require(normalized.size <= 256) { "APP_RULE_LIMIT" }
        return AppRoutingPolicy(mode, normalized.toSortedSet(), quick.toSortedSet())
    }

    fun packageName(raw: String): String {
        require(raw == raw.trim() && raw.length <= 200 && packagePattern.matches(raw)) { "APP_PACKAGE_INVALID" }
        return raw
    }

}

/**
 * Persistence gate for the routing editor (G30).
 *
 * The normalizer keeps an empty INCLUDE_ONLY as a valid in-memory policy because the
 * runtime planner uses it fail-closed. The editor must not persist an empty selection:
 * it keeps the previous policy and asks for at least one application instead.
 */
internal object RoutingSettingsApply {
    fun shouldPersist(mode: AppRoutingMode, selected: Set<String>): Boolean =
        mode != AppRoutingMode.INCLUDE_ONLY || selected.isNotEmpty()
}

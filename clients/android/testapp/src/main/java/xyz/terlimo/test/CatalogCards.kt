package xyz.terlimo.test

internal enum class NodeAvailability { UNKNOWN, AVAILABLE, UNAVAILABLE, TIMEOUT }

internal data class CatalogCardInput(
    val nodeId: String,
    val name: String,
    val country: String,
    val availability: NodeAvailability = NodeAvailability.UNKNOWN,
)

internal data class CatalogCard(
    val nodeId: String,
    val name: String,
    val country: String,
    val availability: NodeAvailability,
    val rttMs: Long?,
    val selected: Boolean,
    val enabled: Boolean,
    val stateText: String,
    val contentDescription: String,
    val minimumTouchTargetDp: Int = 48,
)

internal enum class SelectionProblem { REQUIRED, SAVED_NODE_REMOVED }

internal data class CatalogCards(
    val cards: List<CatalogCard>,
    val selectedNodeId: String?,
    val selectionProblem: SelectionProblem?,
)

/** Immutable UI projection. Wire order is retained and selection is never inferred. */
internal object CatalogCardProjection {
    fun project(
        nodes: List<CatalogCardInput>,
        savedSelectedNodeId: String?,
        pings: Map<String, NodePingState> = emptyMap(),
    ): CatalogCards {
        require(nodes.isNotEmpty()) { "CATALOG_EMPTY" }
        val seen = hashSetOf<String>()
        nodes.forEach {
            require(it.nodeId.isNotBlank() && seen.add(it.nodeId)) { "CATALOG_NODE_INVALID" }
            require(it.name.isNotBlank() && it.name.length <= 100) { "CATALOG_NAME_INVALID" }
            require(it.country.isNotBlank() && it.country.length <= 80) { "CATALOG_COUNTRY_INVALID" }
        }
        val saved = savedSelectedNodeId?.takeIf(String::isNotBlank)
        val selected = saved?.takeIf { id -> nodes.any { it.nodeId == id } }
        val problem = when {
            saved != null && selected == null -> SelectionProblem.SAVED_NODE_REMOVED
            selected == null -> SelectionProblem.REQUIRED
            else -> null
        }
        val cards = nodes.map { node ->
            val ping = pings[node.nodeId] ?: NodePingState.Idle
            val availability = when (ping) {
                is NodePingState.Success -> NodeAvailability.AVAILABLE
                NodePingState.Timeout -> NodeAvailability.TIMEOUT
                NodePingState.Failed -> NodeAvailability.UNAVAILABLE
                else -> node.availability
            }
            val selectedCard = node.nodeId == selected
            val state = buildList {
                add(if (selectedCard) "Выбран" else "Не выбран")
                add(availabilityLabel(availability))
                if (ping is NodePingState.Success) add("${ping.rttMs} мс")
            }.joinToString(", ")
            CatalogCard(
                nodeId = node.nodeId,
                name = node.name,
                country = node.country,
                availability = availability,
                rttMs = (ping as? NodePingState.Success)?.rttMs,
                selected = selectedCard,
                enabled = true,
                stateText = state,
                contentDescription = "${node.country}, ${node.name}, $state",
            )
        }
        return CatalogCards(cards.toList(), selected, problem)
    }

    /** Returns only an explicit available card ID; the caller persists it through existing choose_node. */
    fun explicitSelection(cards: CatalogCards, requestedNodeId: String): String? = cards.cards
        .singleOrNull { it.nodeId == requestedNodeId && it.enabled }
        ?.nodeId

    private fun availabilityLabel(value: NodeAvailability): String = when (value) {
        NodeAvailability.UNKNOWN -> "Доступность не проверена"
        NodeAvailability.AVAILABLE -> "Доступен"
        NodeAvailability.UNAVAILABLE -> "Недоступен"
        NodeAvailability.TIMEOUT -> "Тайм-аут проверки"
    }
}

internal object TerlimoCatalogBrandTokens {
    /**
     * §26.1: the brand tokens resolve against the process-wide theme (set by AppTheme.wrap in
     * each Activity BEFORE views are built), so every existing screen/dialog follows the
     * System/Light/Dark choice. Values are the same brand palette; light mode keeps contrast.
     */
    private val dark = intArrayOf(
        0xFF03070B.toInt(), 0xFF0B1118.toInt(), 0xFF24303D.toInt(), 0xFF00FE7A.toInt(),
        0xFF2D9CFF.toInt(), 0xFFF4FFF9.toInt(), 0xFFA9B4B0.toInt(), 0xFFF4B740.toInt(),
        0xFFFF6B6B.toInt(), 0xFF0B2B20.toInt(), 0xFF1E2A36.toInt(),
    )
    private val light = intArrayOf(
        0xFFF1F5F3.toInt(), 0xFFFFFFFF.toInt(), 0xFFD3DEDD.toInt(), 0xFF008A4B.toInt(),
        0xFF1565C0.toInt(), 0xFF0A1A12.toInt(), 0xFF55655D.toInt(), 0xFFA05A00.toInt(),
        0xFFC62828.toInt(), 0xFFE2F3E8.toInt(), 0xFFE7ECEA.toInt(),
    )
    private val palette: IntArray get() = if (ThemeState.isDark) dark else light

    val BACKGROUND: Int get() = palette[0]
    val SURFACE: Int get() = palette[1]
    val DIVIDER: Int get() = palette[2]
    val ACCENT: Int get() = palette[3]
    val BLUE: Int get() = palette[4]
    val TEXT: Int get() = palette[5]
    val MUTED_TEXT: Int get() = palette[6]
    val WARNING: Int get() = palette[7]
    val ERROR: Int get() = palette[8]
    /** Selected-row / loading-placeholder surfaces; palette-driven for light readability. */
    val SELECTED: Int get() = palette[9]
    val PLACEHOLDER: Int get() = palette[10]
}

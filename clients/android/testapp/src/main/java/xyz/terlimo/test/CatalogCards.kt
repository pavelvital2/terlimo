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
    const val BACKGROUND = 0xFF03070B
    const val SURFACE = 0xFF0B1118
    const val DIVIDER = 0xFF24303D
    const val ACCENT = 0xFF00FE7A
    const val BLUE = 0xFF2D9CFF
    const val TEXT = 0xFFF4FFF9
    const val MUTED_TEXT = 0xFFA9B4B0
    const val WARNING = 0xFFF4B740
    const val ERROR = 0xFFFF6B6B
}

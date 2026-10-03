package xyz.terlimo.test

import org.json.JSONObject

internal data class NodeLabel(val id: String, val name: String, val countryCode: String = "")

internal data class NodeCatalog(
    val nodes: List<NodeLabel>,
    val selectedNodeId: String,
    val revision: String,
)

/** Pure selection rules shared by the TEST host UI and its service. */
internal object NodeSelection {
    const val PLACEHOLDER = "Выберите сервер"

    fun applyNativePhase(state: ViewState, phase: String): ViewState =
        // publishCatalog emits CatalogReady before switch_result. Only the
        // correlated result may complete an active replacement.
        if (state.phase == "Connected" ||
            (state.phase == "SwitchingServer" && phase == "CatalogReady")) state
        else state.copy(phase = phase, error = null)

    /** Apply catalog metadata without changing the active replacement ownership. */
    fun applyCatalog(state: ViewState, catalog: NodeCatalog, summary: CatalogSummary?,
        nativeSelectionAuthoritative: Boolean = false): ViewState {
        val pending = state.pendingNodeId?.takeIf { requested ->
            requested != catalog.selectedNodeId && catalog.nodes.any { it.id == requested }
        }
        val switching = state.phase == "SwitchingServer"
        return state.copy(
            phase = when { switching -> "SwitchingServer"; state.phase == "Connected" -> "Connected"; else -> "CatalogReady" },
            nodes = catalog.nodes,
            // Metadata after a rejected replacement is not a successful switch.
            // Explicit actions/success clear the error; keep last-good feedback.
            error = if (state.phase == "Connected") state.error else null,
            summary = summary,
            selectedNodeId = when {
                switching -> state.selectedNodeId
                nativeSelectionAuthoritative -> catalog.selectedNodeId
                catalog.selectedNodeId.isNotEmpty() -> catalog.selectedNodeId
                // Native may have no saved choice across repeated metadata snapshots.
                // Read only the current display preference, never stale credentials
                // hidden by a fresh browse answer. Native admission is unchanged.
                else -> (if (state.displayMode == CatalogDisplayMode.BROWSE)
                    BrowseCatalogCodec.selectedId(state) else state.selectedNodeId)
                    .takeIf { id -> catalog.nodes.any { it.id == id } }.orEmpty()
            },
            pendingNodeId = if (switching) state.pendingNodeId else pending,
            catalogRevision = catalog.revision,
            pings = state.pings.filterKeys { id -> catalog.nodes.any { it.id == id } },
            // A fresh credential catalog answer switches the display back to credential and
            // drops/ignores the display-only browse branch (owner contract 3/4).
            displayMode = CatalogDisplayMode.CREDENTIAL,
            browseNodes = emptyList(),
            browseLoaded = false,
            browseSelectedId = "",
            browseError = null,
        )
    }

    fun parseCatalog(event: JSONObject): NodeCatalog {
        val entries = event.getJSONArray("nodes")
        check(entries.length() >= 1) { "CATALOG_INVALID" }
        val seen = HashSet<String>()
        val nodes = buildList(entries.length()) {
            repeat(entries.length()) { index ->
                val entry = entries.getJSONObject(index)
                val id = entry.opt("node_id").also { check(it is String) { "CATALOG_INVALID" } } as String
                val name = entry.opt("name").also { check(it is String) { "CATALOG_INVALID" } } as String
                val country = entry.opt("country_code").also { check(it is String) { "CATALOG_INVALID" } } as String
                check(id.isNotEmpty() && seen.add(id)) { "CATALOG_INVALID" }
                // Same bound as the accepted native/wire contract (country_code <= 8) and the
                // browse decoder: an optional public code, never an ISO-2-only assumption.
                check(country.length <= 8) { "CATALOG_INVALID" }
                add(NodeLabel(id, name.take(100), country.uppercase()))
            }
        }
        val selected = event.get("selected_node_id") as? String ?: error("CATALOG_INVALID")
        return NodeCatalog(nodes, selected, event.optString("revision"))
    }

    /** Manual selection is authoritative. Missing/removed IDs never fall back. */
    fun displayedNodeId(nodes: List<NodeLabel>, selectedNodeId: String): String? {
        if (nodes.any { it.id == selectedNodeId }) return selectedNodeId
        return null
    }

    fun spinnerPosition(nodes: List<NodeLabel>, selectedNodeId: String): Int {
        val id = displayedNodeId(nodes, selectedNodeId) ?: return 0
        return nodes.indexOfFirst { it.id == id } + 1
    }

    fun nodeIdAtSpinnerPosition(nodes: List<NodeLabel>, position: Int): String? =
        nodes.getOrNull(position - 1)?.id

    fun connectableNodeId(nodes: List<NodeLabel>, selectedNodeId: String): String? =
        displayedNodeId(nodes, selectedNodeId)
}

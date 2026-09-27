package xyz.terlimo.test

import org.json.JSONObject

/**
 * STEP03.6 owner contract (catalog before access): display-only metadata of the browse branch
 * of GET /gateways (`catalog_mode="browse"`). It is not an admission snapshot: there is no
 * revision, no selected_node_id and no access/transport/probe value, and the host keeps it
 * only in memory for display.
 */
internal data class BrowseCatalog(val nodes: List<NodeLabel>)

/**
 * The CURRENT display branch of the process view (owner contract 3/5), kept separate from the
 * retained verified credentials. A fresh accepted browse answer moves it to BROWSE and a fresh
 * accepted credential catalog answer moves it back to CREDENTIAL; the durable verified cache is
 * never touched by either transition.
 */
internal object CatalogDisplayMode {
    const val CREDENTIAL = "credential"

    /** Same value as the wire `catalog_mode` display branch. */
    const val BROWSE = "browse"
}

/**
 * Strict host-side decode of the owner-contract browse event. Accepted nodes carry exactly the
 * public `node_id`, `name` and optional `country_code`/`region` metadata (the display uses the
 * country only). 0..N nodes are valid; an empty list is a real successful answer, never an error.
 * Optional fields follow the Go browse decode exactly: absent or null metadata is accepted, the
 * known bounds are country_code <= 8 and region <= 64, and a missing country falls back to "".
 */
internal object BrowseCatalogCodec {
    const val MODE = CatalogDisplayMode.BROWSE

    /** Decides the display branch before the strict credential decoder ever sees the event. */
    fun isBrowse(event: JSONObject): Boolean = event.optString("catalog_mode") == MODE

    fun parse(event: JSONObject): BrowseCatalog {
        val entries = event.getJSONArray("nodes")
        val seen = HashSet<String>()
        val nodes = buildList(entries.length()) {
            repeat(entries.length()) { index ->
                val entry = entries.getJSONObject(index)
                val id = entry.opt("node_id").also { check(it is String) { "BROWSE_INVALID" } } as String
                val name = entry.opt("name").also { check(it is String) { "BROWSE_INVALID" } } as String
                val country = optionalText(entry, "country_code")
                check(country.length <= 8) { "BROWSE_INVALID" }
                val region = optionalText(entry, "region")
                check(region.length <= 64) { "BROWSE_INVALID" }
                check(id.isNotEmpty() && seen.add(id)) { "BROWSE_INVALID" }
                add(NodeLabel(id, name.take(100), country.uppercase()))
            }
        }
        return BrowseCatalog(nodes)
    }

    /** Absent or null metadata is accepted as ""; any other non-string value is malformed. */
    private fun optionalText(entry: JSONObject, key: String): String {
        if (!entry.has(key) || entry.isNull(key)) return ""
        return entry.opt(key).also { check(it is String) { "BROWSE_INVALID" } } as String
    }

    /**
     * Host-local display projection of one accepted browse answer. It may update only the
     * in-memory browse fields: the verified `nodes`/selection/revision, the durable verified
     * cache, admission/sync/probe state and the catalogue expectation state machine are never
     * touched, and no CatalogReady is produced.
     */
    fun apply(state: ViewState, browse: BrowseCatalog): ViewState = state.copy(
        browseNodes = browse.nodes,
        browseLoaded = true,
        browseError = null,
        displayMode = CatalogDisplayMode.BROWSE,
    )

    /** True when the browse branch is the current display: fresh answer or explicit offline state. */
    fun isDisplayed(state: ViewState): Boolean =
        state.displayMode == CatalogDisplayMode.BROWSE && (state.browseLoaded || state.browseError != null)

    /**
     * Browse rows are displayed only in the browse display mode, even when stale verified nodes
     * are still present in the view. The retained verified catalog is never shown as browse.
     */
    fun displayedNodes(state: ViewState): List<NodeLabel> =
        if (isDisplayed(state) && state.browseLoaded) state.browseNodes else emptyList()

    /** The host-local preference, exposed only while it still names a displayed browse row. */
    fun selectedId(state: ViewState): String =
        state.browseSelectedId.takeIf { it.isNotEmpty() && displayedNodes(state).any { node -> node.id == it } } ?: ""

    /** Verified rows are connectable material only in the credential display mode. */
    fun verifiedNodes(state: ViewState): List<NodeLabel> =
        if (state.displayMode == CatalogDisplayMode.BROWSE) emptyList() else state.nodes
}

/**
 * The browsed gateway is a preference for the next explicit connect, never an admission.
 * Whenever the current display mode is browse (loading, empty, offline error or a loaded
 * list), a connect from a non-empty browse list requires an explicit row selection; the
 * chosen `gateway_id` then travels as the optional `gateway_key` of the explicit connect
 * command. Only the credential display mode keeps the legacy no-key path.
 */
internal object BrowseConnectGate {
    fun requiresSelection(state: ViewState): Boolean =
        state.displayMode == CatalogDisplayMode.BROWSE && BrowseCatalogCodec.selectedId(state).isEmpty()

    /** The existing pre-admission connect, additionally gated by the browse row selection. */
    fun connectable(state: ViewState, pendingChoice: Boolean): Boolean =
        PreAdmissionConnect.connectable(state, pendingChoice) && !requiresSelection(state)
}

/**
 * Display-only mapping of mirrored accountaccess codes that mean the display catalogue could
 * not be loaded. The native runner keeps retrying on its own cadence; the host only turns the
 * failure into an explicit offline state with a retry affordance instead of an endless loading
 * placeholder, and never invents an empty list.
 */
internal object BrowseErrorPolicy {
    val OFFLINE_CODES = setOf("TRANSPORT", "SERVICE_UNAVAILABLE", "RATE_LIMITED")
    private val WAITING_PHASES = setOf("BootstrapConnecting", "Registering", "ResolvingOperation", "ImportVerified")

    fun fromStderr(code: String, state: ViewState): String? =
        code.takeIf {
            it in OFFLINE_CODES && state.phase in WAITING_PHASES &&
                (state.displayMode == CatalogDisplayMode.BROWSE || state.nodes.isEmpty())
        }
}

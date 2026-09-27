package xyz.terlimo.test

/**
 * Display/selection memory of the last verified catalog. It is NOT an access grant:
 * a Connect always runs a fresh native attempt with admission and readiness, so a
 * cached list can never bypass expiry, revoke or network checks.
 */
internal data class RetainedCatalog(
    val nodes: List<NodeLabel>,
    val selectedNodeId: String,
    val revision: String,
)

/** Preserves the verified list and selection across an ordinary Disconnect. */
internal object SessionRetention {
    fun onStop(state: ViewState, phase: String, error: String?): ViewState =
        state.copy(
            phase = phase,
            wakeRecovery = null,
            error = error,
            pendingNodeId = null,
            pendingSwitchId = null,
            pendingSwitchRevision = null,
            // The attempt is over, so the retained `/me` snapshot is no longer current:
            // keep it for the accepted last-good status/term text, but mark it so
            // account-usage display never presents it as live.
            accountAccess = state.accountAccess?.copy(current = false),
            // The attempt is over: no all-node ping queue may survive it.
            pingAll = PingAllGate.reset(),
        )
}

/** Pure decision from the retained projection. Never grants access by itself. */
internal object RetainedCatalogPolicy {
    /** Node Connect may start for from the disconnected retained state, or null. */
    fun connectableId(phase: String, nodes: List<NodeLabel>, selectedNodeId: String): String? =
        if (phase == "Idle" || phase == "Error") NodeSelection.displayedNodeId(nodes, selectedNodeId) else null

    /**
     * Display-mode-aware projection of the same decision: while the current display is the
     * browse branch, stale verified rows (including a hydrated durable cache) never arm a
     * connect; only the credential display mode exposes them.
     */
    fun connectableId(state: ViewState): String? =
        if (state.displayMode == CatalogDisplayMode.BROWSE) null
        else connectableId(state.phase, state.nodes, state.selectedNodeId)
}

/**
 * Host-side hydration for a fresh process: when the Service does not exist yet the
 * companion view is empty, so the Activity projects the durable cache for display.
 * A current browse display is never replaced by the retained verified credentials.
 * Read-only; it never starts the Service and never grants access.
 */
internal object RetainedProjection {
    fun hydrate(live: ViewState, cacheRaw: String?): ViewState {
        if (live.nodes.isNotEmpty()) return live
        if (live.displayMode == CatalogDisplayMode.BROWSE) return live
        val cached = cacheRaw?.let(CatalogCacheCodec::decode) ?: return live
        return live.copy(nodes = cached.nodes, selectedNodeId = cached.selectedNodeId, catalogRevision = cached.revision)
    }
}

/** Pure UI decision: show the retained catalog read-only after a Disconnect. */
internal object CatalogRenderPolicy {
    fun showRetained(phase: String, nodes: List<NodeLabel>): Boolean =
        (phase == "Idle" || phase == "Error") && nodes.isNotEmpty()

    fun readOnly(phase: String): Boolean = phase == "Idle" || phase == "Error"
}

/** Deterministic, bounded, secret-free text codec for the retained catalog. */
internal object CatalogCacheCodec {
    private const val VERSION = 1
    private const val MAX_BYTES = 64 * 1024

    private fun field(value: String): String {
        require(value.none { it == '\t' || it == '\r' || it == '\n' }) { "CACHE_INVALID" }
        return value
    }

    fun encode(value: RetainedCatalog): String = buildList {
        add("version\t$VERSION")
        add("revision\t${field(value.revision)}")
        add("selected\t${field(value.selectedNodeId)}")
        value.nodes.forEach {
            require(it.id.isNotEmpty() && it.name.isNotEmpty()) { "CACHE_INVALID" }
            add("node\t${field(it.id)}\t${field(it.name)}\t${field(it.countryCode)}")
        }
    }.joinToString("\n", postfix = "\n").also {
        require(it.toByteArray(Charsets.UTF_8).size <= MAX_BYTES) { "CACHE_OVERSIZED" }
    }

    fun decode(raw: String): RetainedCatalog? {
        if (raw.toByteArray(Charsets.UTF_8).size !in 1..MAX_BYTES || !raw.endsWith('\n')) return null
        val rows = raw.dropLast(1).lines().map { it.split('\t') }
        if (rows.any { it.isEmpty() || it[0].isEmpty() }) return null
        val version = rows.singleOrNull { it.size == 2 && it[0] == "version" }?.get(1)?.toIntOrNull() ?: return null
        if (version != VERSION) return null
        if (rows.any { it[0] !in setOf("version", "revision", "selected", "node") }) return null
        val revision = rows.singleOrNull { it.size == 2 && it[0] == "revision" }?.get(1) ?: return null
        val selected = rows.singleOrNull { it.size == 2 && it[0] == "selected" }?.get(1) ?: return null
        val nodes = rows.filter { it[0] == "node" }.map {
            if (it.size != 4) return null
            val id = it[1]; val name = it[2]; val country = it[3]
            if (id.isEmpty() || name.isEmpty() || id.length > 200 || name.length > 100) return null
            if (country.isNotEmpty() && (country.length != 2 || country.any { c -> !c.isUpperCase() })) return null
            NodeLabel(id, name, country)
        }
        if (nodes.isEmpty()) return null
        if (nodes.map { it.id }.distinct().size != nodes.size) return null
        if (selected.isNotEmpty() && nodes.none { it.id == selected }) return null
        return RetainedCatalog(nodes, selected, revision)
    }
}

package xyz.terlimo.test

import org.json.JSONObject

/** Installation-encrypted display preference. Fresh native admission is its only authority. */
internal object MobileSelectionPreference {
    private const val KEY = "mobile_selection"

    fun fromCatalog(state: ViewState, installation: String, source: MobileBootstrapSeed?): JSONObject? {
        if (source == null || state.displayMode != CatalogDisplayMode.CREDENTIAL ||
            state.accountAccess?.current != true) return null
        val id = NodeSelection.displayedNodeId(state.nodes, state.selectedNodeId) ?: return null
        return JSONObject().put("node_id", id)
            .put("installation_id", installation)
            .put("base_url", source.baseUrl).put("environment", source.environment)
            .put("account_ref", state.accountAccess.projection.account.accountRef ?: "")
    }

    // The same AtomicFile update stores the native-acknowledged catalogue and selection;
    // empty native selection removes the preference, so restart cannot resurrect it.
    fun merge(saved: JSONObject, preference: JSONObject?): JSONObject = JSONObject(saved.toString()).also {
        if (preference == null) it.remove(KEY) else it.put(KEY, JSONObject(preference.toString()))
    }

    fun forStart(saved: JSONObject, installation: String, source: MobileBootstrapSeed): JSONObject? {
        val value = saved.optJSONObject(KEY) ?: return null
        if (value.optString("installation_id") != installation ||
            value.optString("base_url") != source.baseUrl ||
            value.optString("environment") != source.environment ||
            value.optString("node_id").isEmpty()) return null
        return JSONObject(value.toString())
    }
}

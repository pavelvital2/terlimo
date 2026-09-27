package xyz.terlimo.test

import android.os.Bundle
import androidx.test.platform.app.InstrumentationRegistry
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Test
import java.util.Base64

/** Reads the existing encrypted state and emits only public catalog identifiers. */
class ExistingCatalogPublicIdsReadOnlyTest {
    private fun publicId(value: String): String {
        check(value.matches(Regex("[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}"))) { "PUBLIC_ID_INVALID" }
        return value
    }

    private fun publicName(value: String): String {
        check(value.length in 1..128 && value.none { it.isISOControl() }) { "PUBLIC_NAME_INVALID" }
        return value
    }

    @Test fun readPublicCatalogIdsOnly() {
        val inst = InstrumentationRegistry.getInstrumentation()
        val saved = InstallationStore(inst.targetContext).read()
        val encoded = saved.getString("state_b64")
        val decoded = Base64.getUrlDecoder().decode(encoded)
        val native = try { JSONObject(decoded.toString(Charsets.UTF_8)) } finally { decoded.fill(0) }
        val catalog = native.getJSONObject("catalog")
        val nodes = catalog.getJSONArray("nodes")
        val publicNodes = JSONArray()
        repeat(nodes.length()) { index ->
            val node = nodes.getJSONObject(index)
            publicNodes.put(JSONObject()
                .put("node_id", publicId(node.getString("node_id")))
                .put("name", publicName(node.getString("name"))))
        }
        val revision = catalog.getString("revision")
        check(revision.matches(Regex("0|[1-9][0-9]{0,19}"))) { "PUBLIC_REVISION_INVALID" }
        val result = JSONObject()
            .put("saved_selected_node_id", native.optString("selected_node_id").let { if (it.isEmpty()) "" else publicId(it) })
            .put("saved_catalog_nodes", publicNodes)
            .put("saved_catalog_revision", revision)
            .put("pending_present", native.has("pending") && !native.isNull("pending"))
            .put("live_catalog_present", SessionService.view.nodes.isNotEmpty())
        inst.sendStatus(2, Bundle().apply { putString("public_state", result.toString()) })
    }
}

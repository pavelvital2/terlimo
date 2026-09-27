package xyz.terlimo.test

import org.json.JSONObject

/**
 * Production envelope for the native `switch_result` frame. The strict allowlist is unchanged:
 * optional fields are accepted only for the failed status, and any extra field is rejected so a
 * malformed frame can never be treated as a valid result.
 */
internal object SwitchResultFrame {
    private val COMMON = setOf("v", "attempt_id", "type", "node_id", "switch_id", "catalog_revision", "status")
    private val FAILED = COMMON + setOf("code", "rollback_allowed")

    fun accepted(event: JSONObject): Boolean {
        val keys = event.keys().asSequence().toSet()
        return keys == COMMON || keys == FAILED
    }
}

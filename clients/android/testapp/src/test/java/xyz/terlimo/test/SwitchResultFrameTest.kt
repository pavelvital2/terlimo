package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** Production decoder contract for the native switch_result frame (strict, not weakened). */
class SwitchResultFrameTest {
    private fun frame(vararg fields: Pair<String, Any>): JSONObject =
        JSONObject().apply {
            put("v", 1); put("attempt_id", "attempt"); put("type", "switch_result")
            put("node_id", "node"); put("switch_id", "switch"); put("catalog_revision", "rev")
            fields.forEach { (key, value) -> put(key, value) }
        }

    @Test fun successAndFailedEnvelopesAreAccepted() {
        assertTrue(SwitchResultFrame.accepted(frame("status" to "ok")))
        assertTrue(SwitchResultFrame.accepted(frame(
            "status" to "failed", "code" to "TRANSPORT_FAILED", "rollback_allowed" to true,
        )))
    }

    @Test fun malformedOrExtendedFramesAreRejected() {
        // Failed without the rollback contract field.
        assertFalse(SwitchResultFrame.accepted(frame("status" to "failed", "code" to "TRANSPORT_FAILED")))
        // Extra field must never be accepted as a valid result.
        assertFalse(SwitchResultFrame.accepted(frame("status" to "failed", "code" to "TRANSPORT_FAILED",
            "rollback_allowed" to true, "raw" to "payload")))
        // Missing required correlation field.
        assertFalse(SwitchResultFrame.accepted(JSONObject().put("v", 1).put("type", "switch_result")
            .put("node_id", "node").put("switch_id", "switch").put("catalog_revision", "rev").put("status", "failed")))
    }
}

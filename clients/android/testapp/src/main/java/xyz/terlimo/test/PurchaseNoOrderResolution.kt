package xyz.terlimo.test

import org.json.JSONObject

/** Prepared under InstallationStore.LOCK, then encrypted and committed once by AtomicFile. */
internal object PurchaseNoOrderResolution {
    fun prepare(state: JSONObject, expected: String, resolved: String, installationId: String): JSONObject {
        check(state.opt("purchase_attempt_state") == expected) { "PURCHASE_NO_CREATE_STALE" }
        val before = PurchaseAttemptCodec.decode(expected) ?: error("PURCHASE_STATE_INVALID")
        val after = PurchaseAttemptCodec.decode(resolved) ?: error("PURCHASE_STATE_INVALID")
        val proof = after.order ?: error("PURCHASE_STATE_INVALID")
        check(before.unresolved && before.order?.paymentEvent == null &&
            proof.outcome in setOf("expired_no_order", "referral_no_order") && proof.noCreateInstallationId == installationId &&
            before.copy(order = before.order?.copy(outcome = proof.outcome,
                noCreateEvent = proof.noCreateEvent, noCreateInstallationId = installationId)) == after) {
            "PURCHASE_NO_CREATE_INVALID"
        }
        val next = JSONObject(state.toString())
        val history = if (next.has("purchase_resolution_history"))
            next.getJSONObject("purchase_resolution_history") else JSONObject()
        check(!history.has(before.attemptId)) { "PURCHASE_NO_CREATE_STALE" }
        history.put(before.attemptId, JSONObject().put("original", expected).put("resolved", resolved))
        return next.put("purchase_resolution_history", history).put("purchase_attempt_state", resolved)
    }
}

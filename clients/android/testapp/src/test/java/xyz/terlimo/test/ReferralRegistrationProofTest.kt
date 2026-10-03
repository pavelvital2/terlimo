package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class ReferralRegistrationProofTest {
    private val account = "12345678-1234-1234-1234-123456789abc"
    private val foreignAccount = "22345678-1234-1234-1234-123456789abc"

    private fun registered(): JSONObject = JSONObject().put("state", "registered")
        .put("account_ref", account).put("fresh_me", JSONObject()
            .put("request_id", "0123456789abcdef0123456789abcdef")
            .put("server_time", "2026-10-03T00:00:00Z")
            .put("schema_version", "1.0").put("status", "ok")
            .put("account_state", "VERIFIED_NO_ENTITLEMENT")
            .put("telegram_linked", true).put("account_ref", account))

    private fun expiry(): JSONObject = JSONObject().put("state", "error")
        .put("code", "REGISTRATION_EXPIRED").put("http_status", 410)
        .put("referral_registration_expired", true)

    @Test fun `fresh verified account may be adopted or matched without data access`() {
        for (state in listOf("VERIFIED_NO_ENTITLEMENT", "VERIFIED_NO_SLOT", "ACTIVE_TRIAL", "ACTIVE_PAID", "EXPIRED")) {
            val event = registered().apply {
                getJSONObject("fresh_me").put("account_state", state)
                    .put("binding_status", "none").put("management_only", true)
                    .put("grant_resolution", JSONObject().put("data_access", "none"))
            }
            assertEquals(state, account, ReferralRegistrationProof.freshAccount(event, null))
            assertEquals(state, account, ReferralRegistrationProof.freshAccount(event, account))
        }
    }

    @Test fun `fresh account must match event and captured account exactly`() {
        assertNull(ReferralRegistrationProof.freshAccount(registered(), foreignAccount))
        assertNull(ReferralRegistrationProof.freshAccount(registered().put("account_ref", foreignAccount), null))
        assertNull(ReferralRegistrationProof.freshAccount(registered().apply {
            getJSONObject("fresh_me").put("account_ref", foreignAccount)
        }, account))
        for (value in listOf<Any>(7, true, JSONObject.NULL, "bad-account")) {
            assertNull(ReferralRegistrationProof.freshAccount(registered().put("account_ref", value), null))
            assertNull(ReferralRegistrationProof.freshAccount(registered().apply {
                getJSONObject("fresh_me").put("account_ref", value)
            }, null))
        }
    }

    @Test fun `unverified revoked or malformed fresh identity cannot prove registration`() {
        for (state in listOf<Any>("UNLINKED", "REVOKED_SESSION", "unknown", 1, true, JSONObject.NULL)) {
            assertNull(ReferralRegistrationProof.freshAccount(registered().apply {
                getJSONObject("fresh_me").put("account_state", state)
            }, null))
        }
        for (linked in listOf<Any>(false, "true", 1, JSONObject.NULL)) {
            assertNull(ReferralRegistrationProof.freshAccount(registered().apply {
                getJSONObject("fresh_me").put("telegram_linked", linked)
            }, null))
        }
        for (value in listOf<Any>("registered", "{}", true, JSONObject.NULL)) {
            assertNull(ReferralRegistrationProof.freshAccount(registered().put("fresh_me", value), null))
        }
        for (state in listOf<Any>("pending", "rejected", "error", true, JSONObject.NULL)) {
            assertNull(ReferralRegistrationProof.freshAccount(registered().put("state", state), null))
        }
    }

    @Test fun `fresh envelope requires exact raw types request id schema and UTC time`() {
        val invalid = mapOf(
            "request_id" to listOf<Any>("ABCDEF0123456789abcdef0123456789", "bad", 123, true, JSONObject.NULL),
            "status" to listOf<Any>("error", "rejected", true, 1, JSONObject.NULL),
            "schema_version" to listOf<Any>("2.0", 1.0, true, JSONObject.NULL),
            "server_time" to listOf<Any>("2026-10-03T00:00:00+00:00", "2026-13-03T00:00:00Z", "bad", 1, JSONObject.NULL),
        )
        for ((field, values) in invalid) for (value in values) {
            assertNull("$field=$value", ReferralRegistrationProof.freshAccount(registered().apply {
                getJSONObject("fresh_me").put(field, value)
            }, null))
        }
        for (field in listOf("request_id", "server_time", "schema_version", "status", "account_ref", "account_state", "telegram_linked")) {
            assertNull(field, ReferralRegistrationProof.freshAccount(registered().apply {
                getJSONObject("fresh_me").remove(field)
            }, null))
        }
        assertEquals(account, ReferralRegistrationProof.freshAccount(registered().apply {
            getJSONObject("fresh_me").put("server_time", "2026-10-03T00:00:00.123Z")
        }, account))
    }

    @Test fun `expiry accepts only explicit integer 410 error marker`() {
        assertEquals(410, ReferralRegistrationProof.expiryStatus(expiry()))
        assertEquals(410, ReferralRegistrationProof.expiryStatus(expiry().put("http_status", 410L)))
        for (status in listOf<Any>("410", 410.0, true, 400, 404, 409, 500, 4_294_967_706L, JSONObject.NULL)) {
            assertNull("$status", ReferralRegistrationProof.expiryStatus(expiry().put("http_status", status)))
        }
        for (marker in listOf<Any>(false, "true", 1, JSONObject.NULL)) {
            assertNull(ReferralRegistrationProof.expiryStatus(expiry().put("referral_registration_expired", marker)))
        }
        assertNull(ReferralRegistrationProof.expiryStatus(expiry().put("code", "TRANSPORT")))
        assertNull(ReferralRegistrationProof.expiryStatus(expiry().put("state", "registered")))
        for (field in listOf("state", "code", "http_status", "referral_registration_expired")) {
            assertNull(field, ReferralRegistrationProof.expiryStatus(expiry().apply { remove(field) }))
        }
    }
}

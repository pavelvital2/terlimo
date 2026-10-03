package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class ReferralTransportGateTest {
    @Test fun coldReadRequiresFreshAccountAndRejectsStaleFlight() {
        val gate = ReferralTransportGate()
        assertTrue(gate.begin("request-a", "info", null, null, null))
        assertFalse(gate.begin("replacement", "info", null, null, null))
        assertNull(gate.dispatch("attempt-a", "account-a"))
        assertTrue(gate.started("attempt-a"))
        assertNull(gate.dispatch("attempt-a", null))
        assertEquals("account-a", gate.dispatch("attempt-a", "account-a")?.expectedAccount)
        assertFalse(gate.accepts("attempt-b", "request-a", "account-a"))
        assertFalse(gate.accepts("attempt-a", "request-b", "account-a"))
        assertFalse(gate.accepts("attempt-a", "request-a", "account-b"))
        assertTrue(gate.accepts("attempt-a", "request-a", "account-a"))
        gate.finish()
        assertFalse(gate.accepts("attempt-a", "request-a", "account-a"))
    }
    @Test fun candidateKeepsKeyAndOnlyDispatchesOnceOnExistingAttempt() {
        val gate = ReferralTransportGate()
        assertTrue(gate.begin("request-a", "set", "original-key", null, "attempt-a"))
        val sent = gate.dispatch("attempt-a", null)!!
        assertEquals("original-key", sent.key)
        assertFalse(sent.cold)
        assertNull(gate.dispatch("attempt-a", null))
        assertTrue(gate.accepts("attempt-a", "request-a", null))
        assertEquals(sent, gate.finish())
    }
    @Test fun knownAccountSwitchCannotDispatchOldOperation() {
        val gate = ReferralTransportGate()
        gate.begin("request-a", "clear", "original-key", "account-a", "attempt-a")
        assertNull(gate.dispatch("attempt-a", "account-b"))
        assertFalse(gate.accepts("attempt-a", "request-a", "account-b"))
    }

    @Test fun anonymousDispatchedFlightCannotResolveAfterAccountAppears() {
        val gate = ReferralTransportGate()
        gate.begin("request", "set", "key", null, "attempt")
        gate.dispatch("attempt", null)
        assertFalse(gate.accepts("attempt", "request", "account-new"))
        assertTrue(gate.accepts("attempt", "request", null))
    }
    @Test fun foregroundOperationTakesOverColdReferralAttempt() {
        val gate = ReferralTransportGate()
        gate.begin("request", "info", null, null, null)
        gate.started("attempt")
        gate.dispatch("attempt", "account")
        gate.relinquishCold()
        assertFalse(gate.finish()!!.cold)
    }
    @Test fun definitiveRejectRequiresIntegerStatusAndExactServerProof() {
        fun frame() = org.json.JSONObject().put("definitive_rejection", true).put("retryable", false).put("http_status", 404)
            .put("request_id", "0123456789abcdef0123456789abcdef").put("code", "REFERRAL_CODE_INVALID")
        assertNotNull(ReferralBridgeProof.rejection(frame(), "install", "original-key"))
        for (status in listOf<Any>("404", 404.5, 503, org.json.JSONObject.NULL)) {
            assertNull(ReferralBridgeProof.rejection(frame().put("http_status", status), "install", "original-key"))
        }
        assertNull(ReferralBridgeProof.rejection(frame().put("code", "TRANSPORT"), "install", "original-key"))
    }

    @Test fun registrationAnonymousFlightAllowsOnlyRegisteredAccountAdoption() {
        val gate = ReferralTransportGate()
        gate.begin("request", "registration", "key", null, "attempt")
        gate.dispatch("attempt", null)
        assertFalse(gate.acceptsRegistration("attempt", "request", "linked", false))
        assertTrue(gate.acceptsRegistration("attempt", "request", "linked", true))
        assertFalse(gate.acceptsRegistration("old-attempt", "request", "linked", true))
        assertFalse(gate.acceptsRegistration("attempt", "other-request", "linked", true))
        gate.finish()
        gate.begin("request", "registration", "key", "original", "attempt")
        gate.dispatch("attempt", "original")
        assertFalse(gate.acceptsRegistration("attempt", "request", "foreign", true))
        gate.finish()
        assertFalse(gate.acceptsRegistration("attempt", "request", "original", true))
    }

    @Test fun retryableOrMissingProofPreservesOriginalCandidateAcrossRestart() {
        val installation = "installation-R1"
        class Disk : ReferralStateStorage {
            var raw: String? = null
            override fun read() = raw?.let { ReferralStateCodec.decode(it, installation) }
            override fun compareAndSet(expectedRevision: Long?, next: ReferralState): Boolean {
                if (read()?.revision != expectedRevision) return false
                raw = ReferralStateCodec.encode(next)
                return true
            }
        }
        for ((status, code) in listOf(404 to "REFERRAL_CODE_INVALID", 400 to "BAD_MESSAGE")) {
            for (retryable in listOf<Any?>(true, null, org.json.JSONObject.NULL, "false", false)) {
                val disk = Disk()
                val journal = ReferralJournal(disk, installation)
                val original = journal.beginPost("OriginalCode", "original-operation-key")
                val frame = org.json.JSONObject().put("definitive_rejection", true)
                    .put("http_status", status).put("code", code)
                    .put("request_id", "0123456789abcdef0123456789abcdef")
                if (retryable != null) frame.put("retryable", retryable)
                val proof = ReferralBridgeProof.rejection(frame, installation, original.idempotencyKey)
                proof?.let { assertTrue(journal.definitiveCandidateReject(it)) }
                val restarted = ReferralJournal(disk, installation)
                if (retryable == false) {
                    assertNotNull(proof)
                    assertNull(restarted.retry())
                    assertEquals(original.idempotencyKey, restarted.state().rejection?.operation?.idempotencyKey)
                } else {
                    assertNull(proof)
                    assertTrue(restarted.state().unresolved)
                    assertEquals(original, restarted.retry())
                    assertEquals("OriginalCode", restarted.retry()?.code)
                    assertNull(restarted.state().rejection)
                }
            }
        }
    }
}

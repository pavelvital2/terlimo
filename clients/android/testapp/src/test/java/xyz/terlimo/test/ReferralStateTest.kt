package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class ReferralStateTest {
    private val installation = "installation-A"
    private val candidate = ReferralCandidate("12345678-1234-1234-1234-123456789abc", "Ab12")
    private val account = "22345678-1234-1234-1234-123456789abc"
    private val otherAccount = "32345678-1234-1234-1234-123456789abc"
    private val key = "referral-key-0001"
    private val registrationKey = "registration-key-0001"
    private val registrationId = "aaaaaaaa-1234-1234-1234-123456789abc"
    private fun pending() = ReferralRegistrationPending(ReferralRegistrationCorrelation(candidate.id, registrationId, registrationKey),
        "tok123", "terlimo_bot", "https://t.me/terlimo_bot?start=tok123", "2026-10-03T00:10:00Z", 600)

    private inner class Disk : ReferralStateStorage {
        var raw: String? = null
        var writes = 0
        var rejectWrite = false
        override fun read(): ReferralState? = raw?.let { ReferralStateCodec.decode(it, installation) }
        override fun compareAndSet(expectedRevision: Long?, next: ReferralState): Boolean {
            if (rejectWrite || read()?.revision != expectedRevision) return false
            raw = ReferralStateCodec.encode(next)
            writes++
            return true
        }
    }
    private fun journal(disk: Disk) = ReferralJournal(disk, installation)
    private fun linked(disk: Disk, expectedAccount: String? = null): ReferralJournal = journal(disk).also {
        it.beginPost("Ab12", key)
        assertTrue(it.acceptPending(key, candidate))
        it.prepareRegistration(registrationKey, expectedAccount)
        assertTrue(it.acceptRegistrationPending(it.captureRegistration(), pending()))
    }
    private fun receipt() = ReferralAttributionReceipt("42345678-1234-1234-1234-123456789abc", account,
        candidate.id, registrationId, registrationKey, "attached")

    @Test fun `post persists original code and key before returning command and survives restart`() {
        val disk = Disk()
        val command = journal(disk).beginPost("Ab12", key)
        assertEquals(1, disk.writes)
        assertEquals(command, disk.read()!!.operation)
        assertEquals(command, journal(disk).retry())
        assertEquals("Ab12", command.code)
        assertEquals(key, command.idempotencyKey)
    }

    @Test fun `failed disk write exposes no sendable operation`() {
        val disk = Disk().apply { rejectWrite = true }
        assertThrows(IllegalStateException::class.java) { journal(disk).beginPost("Ab12", key) }
        assertNull(disk.raw)
        assertNull(journal(disk).retry())
    }

    @Test fun `unknown retains original K and body and blocks new operation and draft replacement`() {
        val disk = Disk()
        val state = journal(disk)
        state.beginPost("Ab12", key)
        assertTrue(state.recordError(key, "TRANSPORT"))
        val restored = journal(disk)
        assertEquals(key, restored.retry()!!.idempotencyKey)
        assertEquals("Ab12", restored.retry()!!.code)
        assertThrows(IllegalStateException::class.java) { restored.beginPost("Other", "referral-key-0002") }
        assertThrows(IllegalStateException::class.java) { restored.beginClear("referral-key-0002") }
        assertThrows(IllegalStateException::class.java) { restored.setDraft("Other") }
    }

    @Test fun `foreign operation result and changed code do not resolve original post`() {
        val disk = Disk()
        val state = journal(disk)
        state.beginPost("Ab12", key)
        val writes = disk.writes
        assertFalse(state.acceptPending("referral-key-9999", candidate))
        assertFalse(state.acceptPending(key, candidate.copy(code = "Other")))
        assertFalse(state.acceptCleared(key))
        assertFalse(state.recordError("referral-key-9999", "TRANSPORT"))
        assertEquals(writes, disk.writes)
        assertNotNull(state.retry())
    }

    @Test fun `server legacy spelling is retained with original local input frozen`() {
        val disk = Disk()
        val state = journal(disk)
        state.beginPost("Ab12", key)
        assertTrue(state.acceptPending(key, candidate.copy(code = "aB12")))
        assertEquals("aB12", journal(disk).state().candidate!!.code)
        assertEquals("Ab12", journal(disk).state().draftCode)
    }

    @Test fun `explicit DELETE has its own original K and retries after restart`() {
        val disk = Disk()
        val state = journal(disk)
        state.beginPost("Ab12", key)
        state.acceptPending(key, candidate)
        val clear = state.beginClear("referral-key-0002")
        assertEquals(ReferralOperationKind.DELETE, clear.kind)
        assertNull(clear.code)
        assertEquals(clear, journal(disk).retry())
        assertFalse(journal(disk).acceptCleared(key))
        assertNotNull(journal(disk).state().candidate)
        assertTrue(journal(disk).acceptCleared(clear.idempotencyKey))
        assertNull(journal(disk).state().candidate)
        assertEquals("", journal(disk).state().draftCode)
    }

    @Test fun `candidate replacement is explicit and old confirmed candidate survives unknown replacement`() {
        val disk = Disk()
        val state = journal(disk)
        state.beginPost("Ab12", key)
        state.acceptPending(key, candidate)
        state.setDraft("Other")
        state.beginPost("Other", "referral-key-0002")
        assertEquals(candidate, journal(disk).state().candidate)
        assertThrows(IllegalStateException::class.java) { state.prepareRegistration(registrationKey) }
    }

    @Test fun `registration snapshot and original token survive restart and lock all candidate edits`() {
        val disk = Disk()
        val state = linked(disk)
        val original = state.state().registration
        val restored = journal(disk)
        assertEquals(original, restored.prepareRegistration(registrationKey))
        val fence = restored.captureRegistration()
        assertFalse(restored.acceptRegistrationPending(fence, pending().copy(
            correlation = pending().correlation.copy(registrationId = "eeeeeeee-1234-1234-1234-123456789abc"))))
        assertFalse(restored.acceptRegistrationPending(fence, pending().copy(token = "token-other", deepLink = "https://t.me/terlimo_bot?start=token-other")))
        assertThrows(IllegalStateException::class.java) { restored.setDraft("Other") }
        assertThrows(IllegalStateException::class.java) { restored.beginClear("referral-key-0002") }
        assertThrows(IllegalStateException::class.java) { restored.prepareRegistration("registration-key-other") }
        assertEquals(original, restored.state().registration)
        assertNotNull(restored.state().candidate)
    }

    @Test fun `optional registration without candidate keeps absent candidate and produces no attribution`() {
        val disk = Disk()
        val state = journal(disk)
        // The keyed journal requires a candidate; ordinary registration uses the unchanged legacy path.
        assertThrows(IllegalStateException::class.java) { state.prepareRegistration(registrationKey) }
        assertNull(disk.raw)
        assertNull(state.state().receipt)
    }

    @Test fun `all stale and foreign receipt dimensions leave candidate and lock untouched`() {
        val disk = Disk()
        val state = linked(disk)
        val proof = receipt()
        val fence = state.captureRegistration()
        val invalid = listOf(proof.copy(registrationId = "dddddddd-1234-1234-1234-123456789abc"),
            proof.copy(idempotencyKey = "registration-key-other"),
            proof.copy(candidateId = "52345678-1234-1234-1234-123456789abc"), proof.copy(accountRef = otherAccount))
        val writes = disk.writes
        invalid.forEach { assertFalse(state.acceptRegistrationReceipt(fence, it, account)) }
        assertFalse(state.acceptRegistrationReceipt(fence.copy(installationId = "installation-foreign"), proof, account))
        assertFalse(state.acceptRegistrationReceipt(fence, proof, otherAccount))
        assertEquals(writes, disk.writes)
        assertTrue(state.state().locked)
        assertEquals(candidate, journal(disk).state().candidate)
    }

    @Test fun `expected account fence rejects switched account even with matching receipt fresh account`() {
        val disk = Disk()
        val state = linked(disk, account)
        assertFalse(state.acceptRegistrationReceipt(state.captureRegistration(), receipt().copy(accountRef = otherAccount), otherAccount))
        assertEquals(candidate, state.state().candidate)
        assertTrue(state.state().locked)
    }

    @Test fun `terminal receipt is durable and repeat is idempotent only for the same account and proof`() {
        val disk = Disk()
        val state = linked(disk)
        val proof = receipt()
        val fence = state.captureRegistration()
        assertTrue(state.acceptRegistrationReceipt(fence, proof, account))
        assertNull(state.state().candidate)
        assertFalse(state.state().unresolved)
        assertEquals(proof, journal(disk).state().receipt)
        val writes = disk.writes
        // A delivered terminal receipt makes the original flight stale; duplicate delivery cannot write again.
        assertFalse(journal(disk).acceptRegistrationReceipt(fence, proof, account))
        assertFalse(journal(disk).acceptRegistrationReceipt(fence, proof, otherAccount))
        assertFalse(journal(disk).acceptRegistrationReceipt(fence, proof.copy(state = "rejected", reason = "self"), account))
        assertEquals(writes, disk.writes)
    }

    @Test fun `semantic reject needs complete correlated receipt and is saved without reward claims`() {
        val disk = Disk()
        val state = linked(disk)
        val rejected = receipt().copy(state = "rejected", reason = "already_attributed")
        assertTrue(state.acceptRegistrationReceipt(state.captureRegistration(), rejected, account))
        assertEquals(rejected, journal(disk).state().receipt)
        assertNull(state.state().candidate)
    }

    @Test fun `stale terminal receipt cannot resolve a new explicit attempt`() {
        val disk = Disk()
        val state = linked(disk)
        val fence = state.captureRegistration()
        state.acceptRegistrationReceipt(fence, receipt(), account)
        state.beginPost("Other", "referral-key-0002")
        assertFalse(state.acceptRegistrationReceipt(fence, receipt(), account))
        assertEquals("Other", state.retry()!!.code)
    }

    @Test fun `corrupt namespace fails closed and cannot replace unresolved operation`() {
        val disk = Disk()
        journal(disk).beginPost("Ab12", key)
        val valid = disk.raw!!
        val bad = listOf("{", valid + "garbage", JSONObject(valid).put("version", 3).toString(),
            JSONObject(valid).put("installation_id", "other").toString(),
            JSONObject(valid).put("revision", "1").toString(), JSONObject(valid).put("candidate", 7).toString(),
            JSONObject(valid).put("extra", true).toString(), JSONObject(valid).apply { remove("operation") }.toString())
        bad.forEach {
            disk.raw = it
            assertThrows(IllegalStateException::class.java) { journal(disk).beginPost("Other", "referral-key-0002") }
            assertEquals(it, disk.raw)
        }
    }

    @Test fun `invalid codes and undersized operation keys are rejected before persistence`() {
        val disk = Disk()
        val state = journal(disk)
        listOf("", "https://terlimo.xyz/?ref=Ab12", "Аb12", "a b", "a".repeat(33)).forEach {
            assertThrows(IllegalStateException::class.java) { state.beginPost(it, key) }
        }
        assertThrows(IllegalStateException::class.java) { state.beginPost("Ab12", "short") }
        assertNull(disk.raw)
    }

    @Test fun `bare code stays unknown but authoritative invalid lookup permits correction durably`() {
        val disk = Disk()
        val state = journal(disk)
        state.beginPost("Ab12", key)
        state.recordError(key, "REFERRAL_CODE_INVALID")
        assertTrue(state.state().unresolved)
        val proof = ReferralCandidateRejection(installation, key, "0123456789abcdef0123456789abcdef",
            404, "REFERRAL_CODE_INVALID")
        assertFalse(state.definitiveCandidateReject(proof.copy(httpStatus = 503)))
        assertFalse(state.definitiveCandidateReject(proof.copy(installationId = "other")))
        assertFalse(state.definitiveCandidateReject(proof.copy(idempotencyKey = "referral-key-9999")))
        assertFalse(state.definitiveCandidateReject(proof.copy(requestId = "bad")))
        assertTrue(state.definitiveCandidateReject(proof))
        val restored = journal(disk)
        assertFalse(restored.state().unresolved)
        assertEquals(key, restored.state().rejection!!.operation.idempotencyKey)
        assertEquals("Ab12", restored.state().rejection!!.operation.code)
        restored.setDraft("Other")
        assertEquals("Other", restored.beginPost("Other", "referral-key-0002").code)
    }

    @Test fun `registration original K is durable before send and unknown replay cannot mint replacement`() {
        val disk = Disk()
        val state = journal(disk)
        state.beginPost("Ab12", key)
        state.acceptPending(key, candidate)
        val original = state.prepareRegistration(registrationKey)
        assertNull(original.registrationId)
        assertNull(original.pending)
        assertEquals(original, disk.read()!!.registration)
        val restored = journal(disk)
        assertEquals(original, restored.retryRegistration())
        assertEquals(original, restored.prepareRegistration(registrationKey))
        val fence = restored.captureRegistration()
        assertTrue(restored.recordRegistrationError(fence, "TRANSPORT"))
        assertEquals(registrationKey, journal(disk).retryRegistration()!!.idempotencyKey)
        assertNull(journal(disk).state().registration!!.registrationId)
        assertThrows(IllegalStateException::class.java) { restored.prepareRegistration("registration-key-new") }
        assertThrows(IllegalStateException::class.java) { restored.beginClear("referral-key-0002") }
    }

    @Test fun `lost pending reply may first replay registered within original candidate K and disk fence`() {
        val disk = Disk()
        val state = journal(disk)
        state.beginPost("Ab12", key)
        state.acceptPending(key, candidate)
        state.prepareRegistration(registrationKey)
        val restored = journal(disk)
        val fence = restored.captureRegistration()
        assertFalse(restored.acceptRegistrationReceipt(fence, receipt().copy(idempotencyKey = "registration-key-other"), account))
        assertTrue(restored.acceptRegistrationReceipt(fence, receipt(), account))
        assertEquals(registrationId, journal(disk).state().registration!!.registrationId)
        assertNull(journal(disk).state().registration!!.pending)
        assertEquals(receipt(), journal(disk).state().receipt)
    }

    @Test fun `disk revision invalidation and write failure prevent old flight receipt resolution`() {
        val disk = Disk()
        val state = linked(disk)
        val old = state.captureRegistration()
        state.recordRegistrationError(old, "TRANSPORT")
        assertFalse(state.acceptRegistrationReceipt(old, receipt(), account))
        val current = state.captureRegistration()
        disk.rejectWrite = true
        assertThrows(IllegalStateException::class.java) { state.acceptRegistrationReceipt(current, receipt(), account) }
        assertEquals(candidate, journal(disk).state().candidate)
        assertNull(journal(disk).state().receipt)
        disk.rejectWrite = false
        assertTrue(state.acceptRegistrationReceipt(current, receipt(), account))
    }

    @Test fun `all pending fields are immutable on same K replay`() {
        val disk = Disk()
        val state = linked(disk)
        val fence = state.captureRegistration()
        val original = pending()
        assertTrue(state.acceptRegistrationPending(fence, original))
        val alternatives = listOf(
            original.copy(botUsername = "other_bot", deepLink = "https://t.me/other_bot?start=tok123"),
            original.copy(expiresAt = "2026-10-03T00:11:00Z"), original.copy(expiresIn = 599),
            original.copy(correlation = original.correlation.copy(idempotencyKey = "registration-key-other")))
        alternatives.forEach { assertFalse(state.acceptRegistrationPending(fence, it)) }
        assertEquals(original, journal(disk).retryRegistration()!!.pending)
    }

    @Test fun `correlated expiry keeps candidate and requires explicit new registration K`() {
        val disk = Disk()
        val state = linked(disk)
        val fence = state.captureRegistration()
        val expiry = ReferralRegistrationExpiry("0123456789abcdef0123456789abcdef", "2026-10-03T00:11:00Z", pending().correlation)
        assertFalse(state.acceptRegistrationExpiry(fence, expiry.copy(correlation = expiry.correlation.copy(
            registrationId = "dddddddd-1234-1234-1234-123456789abc"))))
        assertThrows(IllegalStateException::class.java) { state.acceptRegistrationExpiry(fence, expiry.copy(httpStatus = 409)) }
        assertTrue(state.acceptRegistrationExpiry(fence, expiry))
        val restored = journal(disk)
        assertEquals(candidate, restored.state().candidate)
        assertTrue(restored.state().registration!!.expired)
        assertFalse(restored.state().locked)
        assertNull(restored.retryRegistration())
        assertFalse(restored.acceptRegistrationReceipt(fence, receipt(), account))
        assertThrows(IllegalStateException::class.java) { restored.prepareRegistration(registrationKey) }
        val next = restored.prepareRegistration("registration-key-new")
        assertEquals(candidate.id, next.candidateId)
        assertNull(next.registrationId)
        assertNull(next.pending)
        assertTrue(restored.state().locked)
        assertFalse(restored.acceptRegistrationExpiry(fence, expiry))
    }

    @Test fun `lost pending before expiry can adopt only same candidate K under original disk fence`() {
        val disk = Disk()
        val state = journal(disk)
        state.beginPost("Ab12", key)
        state.acceptPending(key, candidate)
        state.prepareRegistration(registrationKey)
        val fence = state.captureRegistration()
        val expiry = ReferralRegistrationExpiry("0123456789abcdef0123456789abcdef", "2026-10-03T00:11:00Z", pending().correlation)
        assertFalse(state.acceptRegistrationExpiry(fence, expiry.copy(correlation = expiry.correlation.copy(idempotencyKey = "registration-key-other"))))
        assertTrue(state.acceptRegistrationExpiry(fence, expiry))
        assertEquals(registrationId, journal(disk).state().registration!!.registrationId)
        assertEquals(candidate, journal(disk).state().candidate)
    }

    @Test fun `v1 candidate unknown migrates without losing original K body or revision`() {
        val disk = Disk()
        journal(disk).beginPost("Ab12", key)
        val v1 = JSONObject(disk.raw!!).apply { put("version", 1); remove("legacy_state") }
        disk.raw = v1.toString()
        val oldRevision = disk.read()!!.revision
        assertEquals(key, journal(disk).retry()!!.idempotencyKey)
        journal(disk).recordError(key, "TRANSPORT")
        assertEquals(2, JSONObject(disk.raw!!).getInt("version"))
        assertEquals(oldRevision + 1, disk.read()!!.revision)
        assertEquals("Ab12", journal(disk).retry()!!.code)
    }

    @Test fun `v1 pending snapshot without original K is preserved and locked rather than silently replaced`() {
        val disk = Disk()
        val state = journal(disk)
        state.beginPost("Ab12", key)
        state.acceptPending(key, candidate)
        val raw = JSONObject(disk.raw!!).apply {
            put("version", 1); remove("legacy_state")
            put("registration", JSONObject().put("registration_id", "old-client-flight")
                .put("candidate_id", candidate.id).put("token", "old-token").put("expected_account_ref", JSONObject.NULL))
        }.toString()
        disk.raw = raw
        val restored = journal(disk)
        assertTrue(restored.state().legacyLocked)
        assertEquals(raw, restored.state().legacyState)
        assertEquals(candidate, restored.state().candidate)
        assertThrows(IllegalStateException::class.java) { restored.prepareRegistration(registrationKey) }
        assertThrows(IllegalStateException::class.java) { restored.beginClear("referral-key-0002") }
        val encoded = ReferralStateCodec.encode(restored.state())
        assertEquals(raw, ReferralStateCodec.decode(encoded, installation).legacyState)
        assertEquals(raw, disk.raw)
    }

    @Test fun `v1 terminal receipt is retained as legacy evidence without fabricating a registration K`() {
        val disk = Disk()
        val raw = JSONObject(ReferralStateCodec.encode(ReferralState(installation, revision = 5))).apply {
            put("version", 1); remove("legacy_state")
            put("registration", JSONObject().put("registration_id", "old-client-flight")
                .put("candidate_id", candidate.id).put("token", "old-token").put("expected_account_ref", account))
            put("receipt", JSONObject().put("installation_id", installation).put("registration_id", "old-client-flight")
                .put("registration_token", "old-token").put("candidate_id", candidate.id).put("account_ref", account)
                .put("receipt_id", receipt().receiptId).put("state", "attached").put("reason", JSONObject.NULL))
        }.toString()
        disk.raw = raw
        val restored = journal(disk).state()
        assertEquals(5L, restored.revision)
        assertEquals(raw, restored.legacyState)
        assertTrue(restored.legacyLocked)
        assertNull(restored.receipt)
        assertEquals(raw, ReferralStateCodec.decode(ReferralStateCodec.encode(restored), installation).legacyState)
    }
}

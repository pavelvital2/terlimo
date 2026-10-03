package xyz.terlimo.test

import org.json.JSONObject
import org.json.JSONTokener

internal enum class ReferralOperationKind { POST, DELETE }
internal data class ReferralCandidateOperation(
    val kind: ReferralOperationKind,
    val idempotencyKey: String,
    val code: String?,
    val error: String? = null,
)
/** Supplied only by an adapter that observed the correlated authoritative server error. */
internal data class ReferralCandidateRejection(
    val installationId: String,
    val idempotencyKey: String,
    val requestId: String,
    val httpStatus: Int,
    val code: String,
)
internal data class ReferralRejectedCandidateOperation(
    val operation: ReferralCandidateOperation,
    val requestId: String,
    val httpStatus: Int,
    val code: String,
)
internal data class ReferralRegistrationSnapshot(
    val idempotencyKey: String,
    val candidateId: String,
    val registrationId: String? = null,
    val pending: ReferralRegistrationPending? = null,
    val expectedAccountRef: String? = null,
    val expiry: ReferralRegistrationExpiry? = null,
    val error: String? = null,
) {
    val token: String? get() = pending?.token
    val expired: Boolean get() = expiry != null
}
/** Captured from disk immediately before the actor sends the original operation. */
internal data class ReferralRegistrationFence(
    val installationId: String,
    val revision: Long,
    val candidateId: String,
    val idempotencyKey: String,
    val registrationId: String?,
)
internal data class ReferralState(
    val installationId: String,
    val revision: Long = 0,
    val draftCode: String = "",
    val candidate: ReferralCandidate? = null,
    val operation: ReferralCandidateOperation? = null,
    val registration: ReferralRegistrationSnapshot? = null,
    val receipt: ReferralAttributionReceipt? = null,
    val rejection: ReferralRejectedCandidateOperation? = null,
    /** Version1 had no original registration K. Preserve such snapshots; never invent its missing proof. */
    val legacyState: String? = null,
) {
    val legacyLocked: Boolean get() = legacyState != null
    val locked: Boolean get() = legacyLocked || (registration != null && receipt == null && !registration.expired)
    val unresolved: Boolean get() = operation != null || locked
}

/** InstallationStore implements this using the existing encrypted namespace under its lock. */
internal interface ReferralStateStorage {
    fun read(): ReferralState?
    fun compareAndSet(expectedRevision: Long?, next: ReferralState): Boolean
}

/**
 * All transitions persist before exposing a command/snapshot to SessionService. Transport
 * is owned by its existing actor. Unknown results retain the exact original K/body/token.
 * A failed write or disk CAS exposes no new sendable operation.
 */
internal class ReferralJournal(private val storage: ReferralStateStorage, private val installationId: String) {
    @Synchronized fun state(): ReferralState = load().second

    @Synchronized fun setDraft(raw: String): ReferralState = mutate { state ->
        check(!state.unresolved) { "REFERRAL_CANDIDATE_LOCKED" }
        check(raw.isEmpty() || ReferralContract.validCode(raw)) { "REFERRAL_CODE_INVALID" }
        state.copy(draftCode = raw)
    }

    @Synchronized fun beginPost(code: String, idempotencyKey: String): ReferralCandidateOperation {
        check(ReferralContract.validCode(code)) { "REFERRAL_CODE_INVALID" }
        val next = mutate { state ->
            check(!state.unresolved) { "REFERRAL_CANDIDATE_LOCKED" }
            check(ReferralContract.validOperationKey(idempotencyKey)) { "REFERRAL_STATE_INVALID" }
            state.copy(draftCode = code,
                operation = ReferralCandidateOperation(ReferralOperationKind.POST, idempotencyKey, code),
                registration = null, receipt = null, rejection = null)
        }
        return checkNotNull(next.operation)
    }

    @Synchronized fun beginClear(idempotencyKey: String): ReferralCandidateOperation {
        val next = mutate { state ->
            check(!state.unresolved) { "REFERRAL_CANDIDATE_LOCKED" }
            check(ReferralContract.validOperationKey(idempotencyKey)) { "REFERRAL_STATE_INVALID" }
            state.copy(operation = ReferralCandidateOperation(ReferralOperationKind.DELETE, idempotencyKey, null),
                registration = null, receipt = null, rejection = null)
        }
        return checkNotNull(next.operation)
    }

    @Synchronized fun retry(): ReferralCandidateOperation? = state().operation

    @Synchronized fun acceptPending(idempotencyKey: String, candidate: ReferralCandidate): Boolean =
        correlatedMutation { state ->
            val operation = state.operation ?: return@correlatedMutation null
            if (state.locked || operation.kind != ReferralOperationKind.POST ||
                operation.idempotencyKey != idempotencyKey ||
                !candidate.code.equals(operation.code, ignoreCase = true)) return@correlatedMutation null
            state.copy(candidate = candidate, operation = null)
        }

    @Synchronized fun acceptCleared(idempotencyKey: String): Boolean = correlatedMutation { state ->
        val operation = state.operation ?: return@correlatedMutation null
        if (state.locked || operation.kind != ReferralOperationKind.DELETE ||
            operation.idempotencyKey != idempotencyKey) return@correlatedMutation null
        state.copy(draftCode = "", candidate = null, operation = null)
    }

    /** HTTP/transient/native errors do not prove the original operation's terminal outcome. */
    @Synchronized fun recordError(idempotencyKey: String, code: String): Boolean = correlatedMutation { state ->
        val operation = state.operation ?: return@correlatedMutation null
        if (operation.idempotencyKey != idempotencyKey) return@correlatedMutation null
        validText(code)
        state.copy(operation = operation.copy(error = code))
    }

    /** Bare error codes cannot call this seam: original HTTP outcome/provenance is required. */
    @Synchronized fun definitiveCandidateReject(proof: ReferralCandidateRejection): Boolean =
        correlatedMutation { state ->
            val operation = state.operation ?: return@correlatedMutation null
            if (state.locked || operation.kind != ReferralOperationKind.POST ||
                proof.installationId != installationId || proof.idempotencyKey != operation.idempotencyKey ||
                !ReferralContract.validRequestId(proof.requestId) || !definitiveRejection(proof.httpStatus, proof.code)) {
                return@correlatedMutation null
            }
            state.copy(operation = null, rejection = ReferralRejectedCandidateOperation(
                operation.copy(error = proof.code), proof.requestId, proof.httpStatus, proof.code))
        }

    @Synchronized fun prepareRegistration(
        registrationKey: String, expectedAccountRef: String? = null,
    ): ReferralRegistrationSnapshot {
        val next = mutate { state ->
            check(!state.legacyLocked) { "REFERRAL_REGISTRATION_UNAVAILABLE" }
            check(ReferralContract.validOperationKey(registrationKey)) { "REFERRAL_STATE_INVALID" }
            expectedAccountRef?.let { check(ReferralContract.validUuid(it)) { "REFERRAL_STATE_INVALID" } }
            state.registration?.let { original ->
                if (!original.expired) {
                    check(original.idempotencyKey == registrationKey && original.expectedAccountRef == expectedAccountRef &&
                        state.receipt == null) { "REFERRAL_CANDIDATE_LOCKED" }
                    return@mutate state
                }
                check(original.idempotencyKey != registrationKey) { "REGISTRATION_EXPIRED" }
            }
            check(state.operation == null) { "REFERRAL_OPERATION_PENDING" }
            val candidate = checkNotNull(state.candidate) { "REFERRAL_CODE_INVALID" }
            state.copy(registration = ReferralRegistrationSnapshot(registrationKey, candidate.id,
                expectedAccountRef = expectedAccountRef), receipt = null)
        }
        return checkNotNull(next.registration)
    }

    @Synchronized fun retryRegistration(): ReferralRegistrationSnapshot? =
        state().takeIf { !it.legacyLocked && it.receipt == null }?.registration?.takeIf { !it.expired }

    @Synchronized fun captureRegistration(): ReferralRegistrationFence {
        val state = state()
        val registration = checkNotNull(state.registration) { "REFERRAL_REGISTRATION_UNAVAILABLE" }
        check(state.locked && !state.legacyLocked) { "REFERRAL_REGISTRATION_UNAVAILABLE" }
        return ReferralRegistrationFence(installationId, state.revision, registration.candidateId,
            registration.idempotencyKey, registration.registrationId)
    }

    @Synchronized fun recordRegistrationError(fence: ReferralRegistrationFence, code: String): Boolean =
        correlatedMutation { state ->
            if (!matchesFence(state, fence)) return@correlatedMutation null
            validText(code)
            val registration = state.registration ?: return@correlatedMutation null
            state.copy(registration = registration.copy(error = code))
        }

    @Synchronized fun acceptRegistrationPending(
        fence: ReferralRegistrationFence, pending: ReferralRegistrationPending,
    ): Boolean = correlatedMutation { state ->
        if (!matchesFence(state, fence)) return@correlatedMutation null
        ReferralContract.validatePending(pending)
        val registration = checkNotNull(state.registration)
        if (!matchesCorrelation(registration, pending.correlation) ||
            (registration.pending != null && registration.pending != pending)) return@correlatedMutation null
        state.copy(registration = registration.copy(registrationId = pending.correlation.registrationId,
            pending = pending, error = null))
    }

    /** Fresh CURRENT verified /me and actor request/attempt fences are checked by the caller. */
    @Synchronized fun acceptRegistrationReceipt(
        fence: ReferralRegistrationFence, receipt: ReferralAttributionReceipt, freshAccountRef: String,
    ): Boolean =
        correlatedMutation { state ->
            if (!matchesFence(state, fence)) return@correlatedMutation null
            val registration = state.registration ?: return@correlatedMutation null
            ReferralContract.validateReceipt(receipt)
            if (!matchesCorrelation(registration, ReferralRegistrationCorrelation(receipt.candidateId,
                    receipt.registrationId, receipt.idempotencyKey)) || receipt.accountRef != freshAccountRef ||
                (registration.expectedAccountRef != null && receipt.accountRef != registration.expectedAccountRef)) return@correlatedMutation null
            // A lost pending response can first replay as registered: adopt only under the original disk/flight fence.
            state.copy(candidate = null, operation = null, draftCode = "",
                registration = registration.copy(registrationId = receipt.registrationId,
                    expectedAccountRef = receipt.accountRef, error = null), receipt = receipt)
        }

    @Synchronized fun acceptRegistrationExpiry(
        fence: ReferralRegistrationFence, expiry: ReferralRegistrationExpiry,
    ): Boolean = correlatedMutation { state ->
        if (!matchesFence(state, fence)) return@correlatedMutation null
        ReferralContract.validateExpiry(expiry)
        val registration = checkNotNull(state.registration)
        if (!matchesCorrelation(registration, expiry.correlation)) return@correlatedMutation null
        // A terminal, committed same-K expiry never erases the invitation candidate.
        state.copy(registration = registration.copy(registrationId = expiry.correlation.registrationId,
            expiry = expiry, error = "REGISTRATION_EXPIRED"))
    }

    private fun matchesFence(state: ReferralState, fence: ReferralRegistrationFence): Boolean {
        val registration = state.registration ?: return false
        return !state.legacyLocked && state.locked && state.receipt == null &&
            fence.installationId == installationId && state.installationId == fence.installationId &&
            state.revision == fence.revision && state.candidate?.id == fence.candidateId &&
            registration.candidateId == fence.candidateId && registration.idempotencyKey == fence.idempotencyKey &&
            registration.registrationId == fence.registrationId
    }

    private fun matchesCorrelation(snapshot: ReferralRegistrationSnapshot, value: ReferralRegistrationCorrelation): Boolean =
        snapshot.candidateId == value.candidateId && snapshot.idempotencyKey == value.idempotencyKey &&
            (snapshot.registrationId == null || snapshot.registrationId == value.registrationId)

    private fun load(): Pair<ReferralState?, ReferralState> {
        val stored = storage.read()
        val state = stored ?: ReferralState(installationId)
        check(state.installationId == installationId) { "REFERRAL_STATE_INVALID" }
        ReferralStateCodec.validate(state)
        return stored to state
    }

    private fun mutate(change: (ReferralState) -> ReferralState): ReferralState {
        val (stored, current) = load()
        return persist(stored, current, change(current))
    }

    private fun persist(stored: ReferralState?, current: ReferralState, changed: ReferralState): ReferralState {
        if (changed == current && stored != null) return current
        check(current.revision < Long.MAX_VALUE) { "REFERRAL_STATE_INVALID" }
        val next = changed.copy(revision = current.revision + 1)
        ReferralStateCodec.validate(next)
        check(storage.compareAndSet(stored?.revision, next)) { "REFERRAL_STATE_CONFLICT" }
        return next
    }

    private fun correlatedMutation(change: (ReferralState) -> ReferralState?): Boolean {
        val (stored, current) = load()
        val changed = change(current) ?: return false
        persist(stored, current, changed)
        return true
    }
}

/** A corrupt/non-null namespace must throw; treating it as absent could replace unresolved K. */
internal object ReferralStateCodec {
    fun encode(state: ReferralState): String {
        validate(state)
        return JSONObject().put("version", 2).put("installation_id", state.installationId)
            .put("revision", state.revision).put("draft_code", state.draftCode)
            .put("candidate", state.candidate?.let {
                JSONObject().put("id", it.id).put("code", it.code)
            } ?: JSONObject.NULL)
            .put("operation", state.operation?.let {
                JSONObject().put("kind", it.kind.name).put("idempotency_key", it.idempotencyKey)
                    .put("code", it.code ?: JSONObject.NULL).put("error", it.error ?: JSONObject.NULL)
            } ?: JSONObject.NULL)
            .put("registration", state.registration?.let {
                JSONObject().put("idempotency_key", it.idempotencyKey).put("candidate_id", it.candidateId)
                    .put("registration_id", it.registrationId ?: JSONObject.NULL)
                    .put("pending", it.pending?.let(::pendingJson) ?: JSONObject.NULL)
                    .put("expected_account_ref", it.expectedAccountRef ?: JSONObject.NULL)
                    .put("expiry", it.expiry?.let(::expiryJson) ?: JSONObject.NULL).put("error", it.error ?: JSONObject.NULL)
            } ?: JSONObject.NULL)
            .put("receipt", state.receipt?.let(::receiptJson) ?: JSONObject.NULL)
            .put("rejection", state.rejection?.let {
                JSONObject().put("idempotency_key", it.operation.idempotencyKey)
                    .put("original_code", it.operation.code).put("request_id", it.requestId)
                    .put("http_status", it.httpStatus).put("code", it.code)
            } ?: JSONObject.NULL).put("legacy_state", state.legacyState ?: JSONObject.NULL).toString()
    }

    fun decode(raw: String, installationId: String): ReferralState = try {
        val tokener = JSONTokener(raw)
        val json = tokener.nextValue() as JSONObject
        check(tokener.nextClean() == '\u0000')
        val version = json.get("version")
        check(version == 1 || version == 2)
        val baseKeys = setOf("version", "installation_id", "revision", "draft_code", "candidate", "operation", "registration", "receipt", "rejection")
        check(json.keys().asSequence().toSet() == if (version == 1) baseKeys else baseKeys + "legacy_state")
        if (version == 1) validateLegacyRegistration(json)
        val state = ReferralState(
            installationId = text(json, "installation_id"), revision = integer(json, "revision"),
            draftCode = text(json, "draft_code", allowEmpty = true),
            candidate = child(json, "candidate")?.let {
                keys(it, "id", "code"); ReferralCandidate(text(it, "id"), text(it, "code"))
            },
            operation = child(json, "operation")?.let {
                keys(it, "kind", "idempotency_key", "code", "error")
                ReferralCandidateOperation(ReferralOperationKind.valueOf(text(it, "kind")),
                    text(it, "idempotency_key"), nullableText(it, "code"), nullableText(it, "error"))
            },
            registration = if (version == 1) null else child(json, "registration")?.let {
                keys(it, "idempotency_key", "candidate_id", "registration_id", "pending", "expected_account_ref", "expiry", "error")
                ReferralRegistrationSnapshot(text(it, "idempotency_key"), text(it, "candidate_id"), nullableText(it, "registration_id"),
                    child(it, "pending")?.let(::decodePending), nullableText(it, "expected_account_ref"),
                    child(it, "expiry")?.let(::decodeExpiry), nullableText(it, "error"))
            },
            receipt = if (version == 1) null else child(json, "receipt")?.let(ReferralContract::parseReceipt),
            rejection = child(json, "rejection")?.let {
                keys(it, "idempotency_key", "original_code", "request_id", "http_status", "code")
                val status = integer(it, "http_status")
                check(status in 100L..599L)
                val code = text(it, "code")
                ReferralRejectedCandidateOperation(ReferralCandidateOperation(ReferralOperationKind.POST,
                    text(it, "idempotency_key"), text(it, "original_code"), code),
                    text(it, "request_id"), status.toInt(), code)
            },
            legacyState = if (version == 1) {
                if (child(json, "registration") != null || child(json, "receipt") != null) raw else null
            } else nullableRawText(json, "legacy_state"),
        )
        check(state.installationId == installationId)
        validate(state)
        state
    } catch (error: Exception) { throw IllegalStateException("REFERRAL_STATE_INVALID", error) }

    fun validate(state: ReferralState) {
        validText(state.installationId)
        check(state.revision >= 0) { "REFERRAL_STATE_INVALID" }
        check(state.draftCode.isEmpty() || ReferralContract.validCode(state.draftCode)) { "REFERRAL_STATE_INVALID" }
        state.candidate?.let {
            check(ReferralContract.validUuid(it.id) && ReferralContract.validCode(it.code)) { "REFERRAL_STATE_INVALID" }
        }
        state.operation?.let {
            check(ReferralContract.validOperationKey(it.idempotencyKey)) { "REFERRAL_STATE_INVALID" }
            it.error?.let(::validText)
            check(if (it.kind == ReferralOperationKind.POST) it.code != null && ReferralContract.validCode(it.code)
                else it.code == null) { "REFERRAL_STATE_INVALID" }
        }
        state.rejection?.let {
            check(it.operation.kind == ReferralOperationKind.POST &&
                ReferralContract.validOperationKey(it.operation.idempotencyKey) &&
                it.operation.code != null && ReferralContract.validCode(it.operation.code) &&
                it.operation.error == it.code && ReferralContract.validRequestId(it.requestId) &&
                definitiveRejection(it.httpStatus, it.code) && state.operation == null) { "REFERRAL_STATE_INVALID" }
        }
        state.registration?.let {
            check(ReferralContract.validOperationKey(it.idempotencyKey) && ReferralContract.validUuid(it.candidateId)) { "REFERRAL_STATE_INVALID" }
            it.registrationId?.let { id -> check(ReferralContract.validUuid(id)) { "REFERRAL_STATE_INVALID" } }
            it.expectedAccountRef?.let { account -> check(ReferralContract.validUuid(account)) { "REFERRAL_STATE_INVALID" } }
            it.error?.let(::validText)
            check(state.operation == null) { "REFERRAL_STATE_INVALID" }
            if (state.receipt == null) check(it.candidateId == state.candidate?.id) { "REFERRAL_STATE_INVALID" }
            it.pending?.let { pending ->
                ReferralContract.validatePending(pending)
                check(pending.correlation == ReferralRegistrationCorrelation(it.candidateId, checkNotNull(it.registrationId), it.idempotencyKey)) {
                    "REFERRAL_STATE_INVALID"
                }
            }
            it.expiry?.let { expiry ->
                ReferralContract.validateExpiry(expiry)
                check(expiry.correlation == ReferralRegistrationCorrelation(it.candidateId, checkNotNull(it.registrationId), it.idempotencyKey) &&
                    state.receipt == null) { "REFERRAL_STATE_INVALID" }
            }
        }
        state.receipt?.let {
            ReferralContract.validateReceipt(it)
            val registration = checkNotNull(state.registration) { "REFERRAL_STATE_INVALID" }
            check(it.registrationId == registration.registrationId && it.idempotencyKey == registration.idempotencyKey &&
                it.candidateId == registration.candidateId && !registration.expired &&
                it.accountRef == registration.expectedAccountRef && state.candidate == null && state.operation == null) {
                "REFERRAL_STATE_INVALID"
            }
        }
        state.legacyState?.let {
            val legacy = JSONObject(it)
            check(legacy.get("version") == 1 && text(legacy, "installation_id") == state.installationId &&
                (child(legacy, "registration") != null || child(legacy, "receipt") != null) &&
                state.registration == null && state.receipt == null) { "REFERRAL_STATE_INVALID" }
            validateLegacyRegistration(legacy)
        }
    }

    private fun correlationJson(value: ReferralRegistrationCorrelation) = JSONObject()
        .put("candidate_id", value.candidateId).put("registration_id", value.registrationId).put("idempotency_key", value.idempotencyKey)
    private fun pendingJson(value: ReferralRegistrationPending) = JSONObject()
        .put("correlation", correlationJson(value.correlation)).put("token", value.token).put("bot_username", value.botUsername)
        .put("deep_link", value.deepLink).put("expires_at", value.expiresAt).put("expires_in", value.expiresIn)
    private fun expiryJson(value: ReferralRegistrationExpiry) = JSONObject()
        .put("request_id", value.requestId).put("server_time", value.serverTime).put("correlation", correlationJson(value.correlation))
        .put("http_status", value.httpStatus)
    private fun receiptJson(value: ReferralAttributionReceipt) = JSONObject()
        .put("receipt_id", value.receiptId).put("account_ref", value.accountRef).put("candidate_id", value.candidateId)
        .put("registration_id", value.registrationId).put("idempotency_key", value.idempotencyKey)
        .put("state", value.state).put("reason", value.reason ?: JSONObject.NULL)
    private fun decodePending(value: JSONObject): ReferralRegistrationPending {
        keys(value, "correlation", "token", "bot_username", "deep_link", "expires_at", "expires_in")
        val seconds = integer(value, "expires_in")
        check(seconds in 1L..3600L)
        return ReferralRegistrationPending(ReferralContract.parseCorrelation(value.getJSONObject("correlation")),
            text(value, "token"), text(value, "bot_username"), text(value, "deep_link"), text(value, "expires_at"), seconds.toInt())
    }
    private fun decodeExpiry(value: JSONObject): ReferralRegistrationExpiry {
        keys(value, "request_id", "server_time", "correlation", "http_status")
        check(integer(value, "http_status") == 410L)
        return ReferralRegistrationExpiry(text(value, "request_id"), text(value, "server_time"),
            ReferralContract.parseCorrelation(value.getJSONObject("correlation")))
    }
    private fun validateLegacyRegistration(json: JSONObject) {
        val registration = child(json, "registration")
        val receipt = child(json, "receipt")
        registration?.let {
            keys(it, "registration_id", "candidate_id", "token", "expected_account_ref")
            text(it, "registration_id"); nullableText(it, "candidate_id"); nullableText(it, "token"); nullableText(it, "expected_account_ref")
            check(child(json, "operation") == null)
            if (receipt == null) check(nullableText(it, "candidate_id") == child(json, "candidate")?.let { c -> text(c, "id") })
        }
        receipt?.let {
            keys(it, "installation_id", "registration_id", "registration_token", "candidate_id", "account_ref", "receipt_id", "state", "reason")
            val saved = checkNotNull(registration)
            check(text(it, "installation_id") == text(json, "installation_id") &&
                text(it, "registration_id") == text(saved, "registration_id") &&
                text(it, "registration_token") == nullableText(saved, "token") &&
                text(it, "candidate_id") == nullableText(saved, "candidate_id") &&
                text(it, "account_ref") == nullableText(saved, "expected_account_ref") &&
                child(json, "candidate") == null && child(json, "operation") == null)
            check(ReferralContract.validUuid(text(it, "receipt_id")) && ReferralContract.validUuid(text(it, "candidate_id")) &&
                ReferralContract.validUuid(text(it, "account_ref")))
            val state = text(it, "state")
            val reason = nullableText(it, "reason")
            check((state == "attached" && reason == null) || (state == "rejected" && reason in ReferralContract.REJECTION_REASONS))
        }
    }

    private fun child(json: JSONObject, key: String): JSONObject? =
        if (json.get(key) === JSONObject.NULL) null else json.get(key) as JSONObject
    private fun text(json: JSONObject, key: String, allowEmpty: Boolean = false): String {
        val raw = json.get(key)
        check(raw is String)
        if (!allowEmpty) validText(raw)
        return raw
    }
    private fun nullableText(json: JSONObject, key: String): String? =
        if (json.get(key) === JSONObject.NULL) null else text(json, key)
    private fun nullableRawText(json: JSONObject, key: String): String? {
        val raw = json.get(key)
        if (raw === JSONObject.NULL) return null
        check(raw is String && raw.isNotEmpty())
        return raw
    }
    private fun integer(json: JSONObject, key: String): Long {
        val raw = json.get(key)
        check(raw is Int || raw is Long)
        return (raw as Number).toLong()
    }
    private fun keys(json: JSONObject, vararg names: String) {
        check(json.keys().asSequence().toSet() == names.toSet())
    }
}

private fun validText(value: String) {
    check(value.length in 1..512 && value.none { it.code < 32 || it.code == 127 }) { "REFERRAL_STATE_INVALID" }
}
private fun definitiveRejection(httpStatus: Int, code: String): Boolean =
    (httpStatus == 404 && code == "REFERRAL_CODE_INVALID") || (httpStatus == 400 && code == "BAD_MESSAGE")

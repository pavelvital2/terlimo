package xyz.terlimo.test

import org.json.JSONObject

/**
 * Strict host projection of the native->host account_access v1 bridge event.
 *
 * The key set is frozen by account_access.v1.schema.json (access_version 1). The S3-A
 * additive `registration` block (server-owned, display-only, never eligibility truth) is
 * the only accepted extension; no other additive fields may be accepted. Parsing never
 * tears down the attempt; callers reject and keep the last good projection instead.
 */
internal data class AccountAccessProjection(
    val accessVersion: Int,
    val serverTime: String,
    val sessionGeneration: String,
    val previousSessionGeneration: String?,
    val accessRevision: String,
    val account: AccountAccessAccount,
    val entitlement: AccountAccessEntitlement,
    val onboarding: AccountAccessOnboarding,
    val grant: AccountAccessGrant,
    val registration: Registration = Registration("none", false, false, null, false),
    val trial: Trial = Trial("none", false, null, null, null),
) {
    data class Trial(
        val state: String,
        val canActivate: Boolean,
        val reason: String?,
        val startsAt: String?,
        val endsAt: String?,
    )
    data class Registration(
        val state: String,
        val withinHour: Boolean,
        val trialAvailable: Boolean,
        val trialReason: String?,
        val purchaseAvailable: Boolean,
    )
    data class AccountAccessAccount(
        val state: String,
        val telegramLinked: Boolean,
        val bindingStatus: String,
        val managementOnly: Boolean,
        val accountRef: String?,
    )

    data class AccountAccessEntitlement(
        val type: String,
        val status: String,
        val effectiveDeviceLimit: Int,
        val slotsUsed: Int,
        val revision: String,
        val perpetualCommercial: Boolean,
        val validFrom: String?,
        val validUntil: String?,
        val sourceRef: String?,
        // Additive optional active-plan identity (frozen 07.1). null/absent means the plan is
        // unknown; it is never derived from plans/quote/source_ref/catalog.
        val plan: AccountAccessPlan? = null,
    )

    data class AccountAccessPlan(
        val id: String,
        val title: String?,
        val durationCode: String,
    )

    data class AccountAccessOnboarding(
        val state: String,
        val startedBy: String,
        val startedAt: String?,
        val notAfter: String?,
        val durationSeconds: Int,
        val oneTime: Boolean,
        val extendsOnRefresh: Boolean,
        val extendsOnRestart: Boolean,
        val createsTrial: Boolean,
        val requiresHardwareId: Boolean,
        val unit: String,
        val postTelegramIdentity: String,
        val preTelegramReinstall: String,
    )

    data class AccountAccessGrant(
        val controlAvailable: Boolean,
        val restrictedCheckoutAvailable: Boolean,
        val dataAccess: String,
        val effectiveDeadline: String?,
    )
}

internal object AccountAccessParser {
    private val FIELDS = setOf(
        "v", "attempt_id", "type", "access_version", "server_time", "session_generation",
        "previous_session_generation", "access_revision", "account", "entitlement", "onboarding",
        "grant_resolution", "registration", "trial",
    )
    private val ACCOUNT_FIELDS = setOf("state", "telegram_linked", "binding_status", "management_only", "account_ref")
    private val ENTITLEMENT_FIELDS = setOf(
        "type", "status", "effective_device_limit", "slots_used", "revision", "perpetual_commercial",
        "valid_from", "valid_until", "source_ref", "plan",
    )
    private val ENTITLEMENT_REQUIRED = setOf(
        "type", "status", "effective_device_limit", "slots_used", "revision", "perpetual_commercial",
    )
    private val PLAN_FIELDS = setOf("id", "title", "duration_code")
    private val ONBOARDING_FIELDS = setOf(
        "state", "started_by", "started_at", "not_after", "duration_seconds", "one_time",
        "extends_on_refresh", "extends_on_restart", "creates_trial", "requires_hardware_id", "unit",
        "post_telegram_identity", "pre_telegram_reinstall",
    )
    private val GRANT_FIELDS = setOf(
        "control_available", "restricted_checkout_available", "data_access", "effective_deadline",
    )
    private val REGISTRATION_FIELDS = setOf(
        "state", "within_hour", "trial_available", "trial_reason", "purchase_available",
    )
    private val REGISTRATION_STATES = setOf("none", "pending", "registered")
    private val TRIAL_REASONS = setOf(
        "within_hour_no_prior_trial", "hour_expired", "trial_already_used",
    )
    private val TRIAL_FIELDS = setOf("state", "can_activate", "reason", "starts_at", "ends_at")
    private val TRIAL_STATES = setOf("none", "available", "active", "used", "ineligible")
    // The S3-A `registration` and S3-B `trial` blocks are optional additive extensions:
    // an absent block is the older backend and decodes to the default. No other extension
    // is accepted.
    private val OPTIONAL_BLOCKS = setOf("registration", "trial")
    private val ACCOUNT_STATES = setOf(
        "UNLINKED", "VERIFIED_NO_ENTITLEMENT", "VERIFIED_NO_SLOT", "ACTIVE_TRIAL", "ACTIVE_PAID",
        "EXPIRED", "REVOKED_SESSION",
    )
    private val BINDING_STATUSES = setOf("none", "pending", "active", "revoked", "deactivated")
    private val ENTITLEMENT_TYPES = setOf("none", "trial", "paid", "imported")
    private val ENTITLEMENT_STATUSES = setOf("none", "active", "expired", "revoked", "unknown_review")
    private val DATA_ACCESS = setOf("none", "onboarding_hour", "restricted_checkout", "subscription_data")
    private val REVISION = Regex("^(0|[1-9][0-9]{0,18})$")
    private val GENERATION = Regex("^[1-9][0-9]{0,18}$")
    private val UTC_TIME = Regex("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$")

    fun parse(event: JSONObject): AccountAccessProjection {
        val keys = event.keys().asSequence().toSet()
        // The additive S3-A/S3-B blocks are optional; any other extension is rejected.
        check(FIELDS.containsAll(keys) && keys.containsAll(FIELDS - OPTIONAL_BLOCKS)) {
            "ACCOUNT_ACCESS_INVALID"
        }
        check(event.getInt("v") == 1 && event.getString("type") == "account_access" &&
            event.getInt("access_version") == 1) { "ACCOUNT_ACCESS_INVALID" }
        val serverTime = event.getString("server_time")
        val generation = event.getString("session_generation")
        val previous = nullableString(event, "previous_session_generation")
        val accessRevision = event.getString("access_revision")
        check(utcTime(serverTime) && generation.matches(GENERATION) &&
            (previous == null || previous.matches(GENERATION)) && accessRevision.matches(REVISION)) {
            "ACCOUNT_ACCESS_INVALID"
        }

        val account = event.getJSONObject("account")
        check(account.keys().asSequence().toSet() == ACCOUNT_FIELDS) { "ACCOUNT_ACCESS_INVALID" }
        check(account.getString("state") in ACCOUNT_STATES &&
            account.getString("binding_status") in BINDING_STATUSES) { "ACCOUNT_ACCESS_INVALID" }

        val entitlement = event.getJSONObject("entitlement")
        val entitlementKeys = entitlement.keys().asSequence().toSet()
        check(entitlementKeys.containsAll(ENTITLEMENT_REQUIRED) && ENTITLEMENT_FIELDS.containsAll(entitlementKeys)) {
            "ACCOUNT_ACCESS_INVALID"
        }
        check(entitlement.getString("type") in ENTITLEMENT_TYPES &&
            entitlement.getString("status") in ENTITLEMENT_STATUSES &&
            entitlement.getString("revision").matches(REVISION)) { "ACCOUNT_ACCESS_INVALID" }
        val limit = deviceCount(entitlement, "effective_device_limit")
        val slots = deviceCount(entitlement, "slots_used")
        val validFrom = nullableString(entitlement, "valid_from")
        val validUntil = nullableString(entitlement, "valid_until")
        val sourceRef = nullableString(entitlement, "source_ref")
        check((validFrom == null || utcTime(validFrom)) && (validUntil == null || utcTime(validUntil)) &&
            (sourceRef == null || sourceRef.length <= 128)) { "ACCOUNT_ACCESS_INVALID" }
        // Additive optional plan: absent or JSON null means unknown. When present it must be
        // the exact frozen object; title may be null, and no unknown nested key is accepted.
        val plan = if (!entitlement.has("plan") || entitlement.isNull("plan")) {
            null
        } else {
            val block = entitlement.getJSONObject("plan")
            check(block.keys().asSequence().toSet() == PLAN_FIELDS) { "ACCOUNT_ACCESS_INVALID" }
            val planId = block.getString("id")
            val planTitle = nullableString(block, "title")
            val durationCode = block.getString("duration_code")
            check(planId.isNotEmpty() && planId.length <= 128 &&
                (planTitle == null || planTitle.length <= 128) &&
                durationCode.isNotEmpty() && durationCode.length <= 64) {
                "ACCOUNT_ACCESS_INVALID"
            }
            AccountAccessProjection.AccountAccessPlan(planId, planTitle, durationCode)
        }

        val onboarding = event.getJSONObject("onboarding")
        check(onboarding.keys().asSequence().toSet() == ONBOARDING_FIELDS) { "ACCOUNT_ACCESS_INVALID" }
        val onboardingState = onboarding.getString("state")
        val startedAt = nullableString(onboarding, "started_at")
        val notAfter = nullableString(onboarding, "not_after")
        check(onboarding.getString("started_by") == "server_confirmed_first_connection" &&
            onboarding.getInt("duration_seconds") == 3600 &&
            onboarding.getBoolean("one_time") && !onboarding.getBoolean("extends_on_refresh") &&
            !onboarding.getBoolean("extends_on_restart") && !onboarding.getBoolean("creates_trial") &&
            !onboarding.getBoolean("requires_hardware_id") &&
            onboarding.getString("unit") == "installation_fingerprint" &&
            onboarding.getString("post_telegram_identity") == "account_history_correlation" &&
            onboarding.getString("pre_telegram_reinstall") ==
                "may_be_indistinguishable_new_key_separate_unit") { "ACCOUNT_ACCESS_INVALID" }
        when (onboardingState) {
            "not_started" -> check(startedAt == null && notAfter == null) { "ACCOUNT_ACCESS_INVALID" }
            "active", "expired" -> check(startedAt != null && notAfter != null && utcTime(startedAt) && utcTime(notAfter)) {
                "ACCOUNT_ACCESS_INVALID"
            }
            else -> error("ACCOUNT_ACCESS_INVALID")
        }

        val grant = event.getJSONObject("grant_resolution")
        check(grant.keys().asSequence().toSet() == GRANT_FIELDS) { "ACCOUNT_ACCESS_INVALID" }
        val dataAccess = grant.getString("data_access")
        val deadline = nullableString(grant, "effective_deadline")
        check(grant.getBoolean("control_available") && grant.getBoolean("restricted_checkout_available") &&
            dataAccess in DATA_ACCESS) { "ACCOUNT_ACCESS_INVALID" }
        when (dataAccess) {
            "onboarding_hour" -> check(deadline != null && utcTime(deadline)) { "ACCOUNT_ACCESS_INVALID" }
            "subscription_data" -> {
                // Mirrors the accepted Go predicate (accountaccess.IndefiniteSubscriptionData):
                // only a perpetual commercial entitlement without valid_until may omit the finite
                // deadline. Any other null deadline stays malformed and fail-closed; the verified
                // catalog validity and the admitted node lease remain the finite bounds.
                val perpetual = entitlement.getBoolean("perpetual_commercial")
                if (deadline == null) {
                    check(indefiniteSubscriptionData(dataAccess, perpetual, validUntil)) {
                        "ACCOUNT_ACCESS_INVALID"
                    }
                } else {
                    check(utcTime(deadline)) { "ACCOUNT_ACCESS_INVALID" }
                }
            }
            else -> check(deadline == null) { "ACCOUNT_ACCESS_INVALID" }
        }

        val registration = if (event.has("registration")) {
            val block = event.getJSONObject("registration")
            check(block.keys().asSequence().toSet() == REGISTRATION_FIELDS) { "ACCOUNT_ACCESS_INVALID" }
            val registrationState = block.getString("state")
            val trialReason = nullableString(block, "trial_reason")
            check(registrationState in REGISTRATION_STATES &&
                (trialReason == null || trialReason in TRIAL_REASONS)) { "ACCOUNT_ACCESS_INVALID" }
            AccountAccessProjection.Registration(
                state = registrationState,
                withinHour = block.getBoolean("within_hour"),
                trialAvailable = block.getBoolean("trial_available"),
                trialReason = trialReason,
                purchaseAvailable = block.getBoolean("purchase_available"),
            )
        } else {
            AccountAccessProjection.Registration("none", false, false, null, false)
        }

        val trial = if (event.has("trial")) {
            val block = event.getJSONObject("trial")
            check(block.keys().asSequence().toSet() == TRIAL_FIELDS) { "ACCOUNT_ACCESS_INVALID" }
            val trialState = block.getString("state")
            val reason = nullableString(block, "reason")
            val startsAt = nullableString(block, "starts_at")
            val endsAt = nullableString(block, "ends_at")
            check(trialState in TRIAL_STATES && (reason == null || reason.length <= 64) &&
                (startsAt == null || utcTime(startsAt)) && (endsAt == null || utcTime(endsAt)) &&
                (trialState != "active" || (startsAt != null && endsAt != null))) {
                "ACCOUNT_ACCESS_INVALID"
            }
            AccountAccessProjection.Trial(
                state = trialState,
                canActivate = block.getBoolean("can_activate"),
                reason = reason,
                startsAt = startsAt,
                endsAt = endsAt,
            )
        } else {
            AccountAccessProjection.Trial("none", false, null, null, null)
        }

        return AccountAccessProjection(
            accessVersion = 1,
            serverTime = serverTime,
            sessionGeneration = generation,
            previousSessionGeneration = previous,
            accessRevision = accessRevision,
            account = AccountAccessProjection.AccountAccessAccount(
                state = account.getString("state"),
                telegramLinked = account.getBoolean("telegram_linked"),
                bindingStatus = account.getString("binding_status"),
                managementOnly = account.getBoolean("management_only"),
                accountRef = nullableString(account, "account_ref"),
            ),
            entitlement = AccountAccessProjection.AccountAccessEntitlement(
                type = entitlement.getString("type"),
                status = entitlement.getString("status"),
                effectiveDeviceLimit = limit,
                slotsUsed = slots,
                revision = entitlement.getString("revision"),
                perpetualCommercial = entitlement.getBoolean("perpetual_commercial"),
                validFrom = validFrom,
                validUntil = validUntil,
                sourceRef = sourceRef,
                plan = plan,
            ),
            onboarding = AccountAccessProjection.AccountAccessOnboarding(
                state = onboardingState,
                startedBy = onboarding.getString("started_by"),
                startedAt = startedAt,
                notAfter = notAfter,
                durationSeconds = onboarding.getInt("duration_seconds"),
                oneTime = onboarding.getBoolean("one_time"),
                extendsOnRefresh = onboarding.getBoolean("extends_on_refresh"),
                extendsOnRestart = onboarding.getBoolean("extends_on_restart"),
                createsTrial = onboarding.getBoolean("creates_trial"),
                requiresHardwareId = onboarding.getBoolean("requires_hardware_id"),
                unit = onboarding.getString("unit"),
                postTelegramIdentity = onboarding.getString("post_telegram_identity"),
                preTelegramReinstall = onboarding.getString("pre_telegram_reinstall"),
            ),
            grant = AccountAccessProjection.AccountAccessGrant(
                controlAvailable = grant.getBoolean("control_available"),
                restrictedCheckoutAvailable = grant.getBoolean("restricted_checkout_available"),
                dataAccess = dataAccess,
                effectiveDeadline = deadline,
            ),
            registration = registration,
            trial = trial,
        )
    }

    private fun nullableString(source: JSONObject, key: String): String? {
        if (!source.has(key) || source.isNull(key)) return null
        val value = source.get(key)
        check(value is String) { "ACCOUNT_ACCESS_INVALID" }
        return value
    }

    /**
     * Contract parity with accountaccess/contract.go IndefiniteSubscriptionData: a null
     * effective_deadline is the indefinite shape only for subscription_data with a perpetual
     * commercial entitlement that carries no valid_until.
     */
    private fun indefiniteSubscriptionData(dataAccess: String, perpetualCommercial: Boolean, validUntil: String?): Boolean =
        dataAccess == "subscription_data" && perpetualCommercial && validUntil == null

    private fun utcTime(value: String): Boolean = value.matches(UTC_TIME)
    /** Server/Android Int32 capacity; no commercial cap or slots <= limit assumption. */
    private fun deviceCount(source: JSONObject, key: String): Int {
        val value = source.opt(key)
        check(value is Int || value is Long) { "ACCOUNT_ACCESS_INVALID" }
        val count = (value as Number).toLong()
        check(count in 0..Int.MAX_VALUE.toLong()) { "ACCOUNT_ACCESS_INVALID" }
        return count.toInt()
    }

}

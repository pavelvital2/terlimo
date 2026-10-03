package xyz.terlimo.test

/** One OS-owned VPN service instance: unlock may release one start, never a retry loop. */
class AlwaysOnStartGate {
    enum class Decision { IGNORE, WAIT_UNLOCK, START, REFUSE }
    private var delivered = false
    private var closed = false
    @Synchronized fun start(appOrigin: Boolean, systemMode: Boolean, unlocked: Boolean,
        consent: Boolean): Decision {
        if (appOrigin || closed || delivered) return Decision.IGNORE
        if (!systemMode || !consent) { delivered = true; return Decision.REFUSE }
        if (!unlocked) return Decision.WAIT_UNLOCK
        delivered = true
        return Decision.START
    }
    @Synchronized fun close() { closed = true }
    @Synchronized fun isOpen(): Boolean = !closed
}

/** Explicit origin, not an Activity launch preference or a forged user token. */
internal object AutoConnectOriginPolicy {
    fun allowed(systemOrigin: Boolean, systemCurrent: Boolean, userEnabled: Boolean,
        userTokenCurrent: Boolean): Boolean =
        if (systemOrigin) systemCurrent else userEnabled && userTokenCurrent
    fun userUpdate(systemOrigin: Boolean, jobOnly: Boolean, physicalRecovery: Boolean): Boolean =
        !systemOrigin && !jobOnly && !physicalRecovery
    fun explicitBusinessConnect(systemOrigin: Boolean): Boolean = !systemOrigin
    fun selectCommand(nodeId: String, systemOrigin: Boolean): org.json.JSONObject =
        org.json.JSONObject().put("type", "select_node").put("node_id", nodeId)
            .put("explicit_connect", explicitBusinessConnect(systemOrigin))
}

/** Admission precheck only; native grant/lease validation remains authoritative. */
internal object AlwaysOnAccess {
    fun usable(snapshot: AccountAccessSnapshot?, now: Long): Boolean {
        if (snapshot?.current != true || snapshot.projection.account.accountRef.isNullOrBlank()) return false
        if (AccountAccessPolicy.onboardingHourBlockedReason(snapshot) != null) return false
        if (snapshot.projection.account.managementOnly) return false
        return when (snapshot.projection.grant.dataAccess) {
            "onboarding_hour" -> AccountAccessPolicy.isConfirmedOnboardingHour(snapshot, now)
            "subscription_data" -> snapshot.projection.account.state in setOf("ACTIVE_PAID", "ACTIVE_TRIAL") &&
                snapshot.projection.entitlement.status == "active" &&
                (AccountAccessPolicy.remainingMillis(snapshot, now)?.let { it > 0 } ?: true)
            else -> false
        }
    }
}

/** Guard runs before storage construction/read; callers cannot interpret locked CE as empty. */
internal inline fun <T> withUnlockedStorage(unlocked: Boolean, access: () -> T): T {
    check(unlocked) { "USER_UNLOCK_REQUIRED" }
    return access()
}

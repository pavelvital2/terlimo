package xyz.terlimo.test

/**
 * STEP03.6 waiting-for-explicit-first-connect state.
 *
 * The first-connect state exists only for the authoritative /me snapshot that combines
 * `data_access=none` with `onboarding.state=not_started`. This is a valid recovery of an
 * existing attempt as well as a fresh installation, so the UI may offer the Connect
 * action without a node selection. Every existing exclusion wins: an ineligible state
 * (revoked session, revoked/deactivated binding, revoked/unknown-review entitlement)
 * never enters this branch, and malformed/expired-hour snapshots are rejected earlier by
 * the strict projection parser. Background/resume/install never touch this state: only
 * the explicit after-consent action arms the native command.
 */
internal object PreAdmissionConnect {
    /**
     * Owner wording of the pre-admission surfaces (23.09 clarifications). Display-only,
     * descriptive text: it states the purpose of the first hour and the registration
     * deadline; it implements no eligibility, trial activation or payment path.
     */
    const val HOUR_PURPOSE =
        "1 час доступа для регистрации. После регистрации вы сможете выбрать пробный доступ на 7 дней или купить подписку"

    /** Explicit warning shown before the first hour starts. */
    const val HOUR_WARNING =
        "У вас будет 1 час для регистрации. Если не успеете зарегистрироваться, пробные 7 дней будут недоступны. " +
            "Продолжить можно будет после покупки подписки; регистрация останется обязательной"

    private val PROHIBITED_ACCOUNT_STATES = setOf("REVOKED_SESSION", "EXPIRED")
    private val PROHIBITED_BINDING_STATUSES = setOf("revoked", "deactivated")
    private val PROHIBITED_ENTITLEMENT_STATUSES = setOf("expired", "revoked", "unknown_review")
    private val ACTIVE_OR_STOPPING_PHASES = setOf(
        "Connected", "NodeAuthenticating", "ConfiguringVPN", "SwitchingServer", "Reconnecting",
        "SleepPaused", "KillSwitch", "Stopping",
    )

    fun eligible(state: ViewState): Boolean {
        if (state.phase in ACTIVE_OR_STOPPING_PHASES) return false
        val projection = state.accountAccess?.projection ?: return false
        if (projection.grant.dataAccess != "none") return false
        if (projection.onboarding.state != "not_started") return false
        if (projection.account.state in PROHIBITED_ACCOUNT_STATES) return false
        if (projection.account.bindingStatus in PROHIBITED_BINDING_STATUSES) return false
        if (projection.entitlement.status in PROHIBITED_ENTITLEMENT_STATUSES) return false
        return true
    }

    /** Existing data grant connects the selected public row through ordinary admission. */
    fun activeBrowse(state: ViewState): Boolean {
        if (state.phase in ACTIVE_OR_STOPPING_PHASES || state.displayMode != CatalogDisplayMode.BROWSE) return false
        val snapshot = state.accountAccess ?: return false
        val p = snapshot.projection
        return snapshot.current && p.grant.dataAccess in setOf("subscription_data", "onboarding_hour") &&
            p.account.state !in PROHIBITED_ACCOUNT_STATES &&
            p.account.bindingStatus !in PROHIBITED_BINDING_STATUSES &&
            p.entitlement.status !in PROHIBITED_ENTITLEMENT_STATUSES &&
            BrowseCatalogCodec.selectedId(state).isNotEmpty()
    }

    /**
     * The explicit Connect without a node selection is permitted only in the eligible
     * first-connect state and only while no choice is pending. It performs no I/O.
     */
    fun connectable(state: ViewState, pendingChoice: Boolean): Boolean =
        !pendingChoice && state.pendingNodeId == null && (eligible(state) || activeBrowse(state))
}

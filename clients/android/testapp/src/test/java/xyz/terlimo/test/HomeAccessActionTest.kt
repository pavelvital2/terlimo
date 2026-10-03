package xyz.terlimo.test
import org.junit.Assert.assertEquals
import org.junit.Test
class HomeAccessActionTest {
    private fun projection(
        dataAccess: String = "none",
        onboarding: String = "not_started",
        accountState: String = "UNLINKED",
        bindingStatus: String = "none",
        entitlementStatus: String = "none",
        managementOnly: Boolean = false,
    ) = AccountAccessProjection(
        accessVersion = 1,
        serverTime = "2026-09-23T02:00:00Z",
        sessionGeneration = "1",
        previousSessionGeneration = null,
        accessRevision = "1",
        account = AccountAccessProjection.AccountAccessAccount(
            state = accountState, telegramLinked = false, bindingStatus = bindingStatus,
            managementOnly = managementOnly, accountRef = null),
        entitlement = AccountAccessProjection.AccountAccessEntitlement(
            type = "none", status = entitlementStatus, effectiveDeviceLimit = 1, slotsUsed = 0,
            revision = "1", perpetualCommercial = false, validFrom = null, validUntil = null, sourceRef = null),
        onboarding = AccountAccessProjection.AccountAccessOnboarding(
            state = onboarding, startedBy = "server_confirmed_first_connection", startedAt = null,
            notAfter = null, durationSeconds = 3600, oneTime = true, extendsOnRefresh = false,
            extendsOnRestart = false, createsTrial = false, requiresHardwareId = false,
            unit = "installation_fingerprint", postTelegramIdentity = "account_history_correlation",
            preTelegramReinstall = "may_be_indistinguishable_new_key_separate_unit"),
        grant = AccountAccessProjection.AccountAccessGrant(
            controlAvailable = true, restrictedCheckoutAvailable = true,
            dataAccess = dataAccess, effectiveDeadline = null),
    )

    private fun state(projection: AccountAccessProjection?) = ViewState(
        accountAccess = projection?.let { AccountAccessSnapshot(it, 0L, AccountAccessChain()) })


    @Test fun `unconfirmed account offers explicit hour then registration`() {
        assertEquals(HomeAccessAction.NONE, HomeAccessAction.forState(state(null), false))
        assertEquals(HomeAccessAction.HOUR, HomeAccessAction.forState(state(projection()), false))
        assertEquals(HomeAccessAction.REGISTER, HomeAccessAction.forState(state(projection()), true))
        assertEquals(HomeAccessAction.REGISTER, HomeAccessAction.forState(state(projection(onboarding = "expired")), false))
        assertEquals(HomeAccessAction.REGISTER, HomeAccessAction.forState(state(projection(dataAccess = "onboarding_hour", onboarding = "active")), false))
    }
    @Test fun `confirmed account uses server trial and ordinary active connection`() {
        val base = projection(onboarding = "active")
        val registered = base.copy(account = base.account.copy(telegramLinked = true, bindingStatus = "active", accountRef = "account-1"))
        val trial = registered.copy(trial = registered.trial.copy(canActivate = true))
        assertEquals(HomeAccessAction.TRIAL, HomeAccessAction.forState(state(trial), false))
        assertEquals(HomeAccessAction.PURCHASE, HomeAccessAction.forState(state(registered), false))
        assertEquals(HomeAccessAction.PURCHASE, HomeAccessAction.forState(state(trial).copy(trial = TrialState(error = "TRIAL_ALREADY_USED")), false))
        val active = trial.copy(grant = trial.grant.copy(dataAccess = "subscription_data"))
        assertEquals(HomeAccessAction.NONE, HomeAccessAction.forState(state(active), false))
        assertEquals(HomeAccessAction.NONE, HomeAccessAction.forState(state(trial).copy(recoveryStatus = "RECOVERY_RUNNING"), false))
    }
}

package xyz.terlimo.test
import org.junit.Assert.*
import org.junit.Test
class ReferralClientPresentationTest {
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



    private fun registered(account: String): ViewState {
        val p = projection(onboarding = "expired", entitlementStatus = "expired")
        return state(p.copy(account = p.account.copy(accountRef = account, telegramLinked = true, bindingStatus = "deactivated")))
    }
    private fun info(account: String) = ReferralInfo(account, "OwnCode", "https://t.me/terlimo_vpn_wdtt_bot?start=ref_uOwnCode",
        "https://terlimo.xyz/?ref=uOwnCode", ReferralAttribution("none", null, null), 3L,
        ReferralDiscount("RUB",10000L,"eligible"), 7L, 0L, "referral-20261003-v1")

    @Test fun expiredRegisteredAccountCanDisplayOwnServerCodeButForeignAccountCannot() {
        val stored = ReferralClientState(info = info("account-a"))
        val allowed = ReferralClientPresentation.model(registered("account-a").copy(referral = stored))
        assertEquals("OwnCode", allowed.ownCode)
        assertEquals(ReferralInfoUiState.READY, allowed.infoState)
        val foreign = ReferralClientPresentation.model(registered("account-b").copy(referral = stored))
        assertNull(foreign.ownCode)
        assertNull(foreign.telegramLink)
        assertNull(ReferralClientPresentation.model(ViewState(referral = stored)).ownCode)
    }
    @Test fun candidateUnknownKeepsOriginalCodeAndOnlyExplicitRetry() {
        val durable = ReferralState("install", draftCode = "InviteCode", operation = ReferralCandidateOperation(
            ReferralOperationKind.POST, "referral-key-0001", "InviteCode", "TRANSPORT"))
        val model = ReferralClientPresentation.model(state(projection()).copy(referral = ReferralClientState(durable)))
        assertEquals("InviteCode", model.candidateCode)
        assertEquals(ReferralCandidateUiState.UNKNOWN, model.candidateState)
        assertTrue(model.canRetry)
        assertFalse(model.canSubmit)
        assertFalse(model.canClear)
        assertFalse(model.canEdit)
        assertNull(model.ownCode)
    }
    @Test fun acceptedCandidateNeverMeansAccountAttributionSucceeded() {
        val durable = ReferralState("install", draftCode = "InviteCode", candidate = ReferralCandidate("candidate", "InviteCode"))
        val model = ReferralClientPresentation.model(state(projection()).copy(referral = ReferralClientState(durable)))
        assertEquals(ReferralCandidateUiState.PENDING, model.candidateState)
        assertNull(model.ownCode)
        assertTrue(model.statusMessage!!.contains("ещё не подтверждена"))
    }

    @Test fun oldAccountReceiptDoesNotConfirmAttributionForCurrentAccount() {
        val receipt = ReferralAttributionReceipt("receipt", "account-a", "candidate", "reg", "registration-key-0001", "attached", null)
        val stored = ReferralClientState(durable = ReferralState("install", receipt = receipt))
        val model = ReferralClientPresentation.model(registered("account-b").copy(referral = stored))
        assertEquals(ReferralCandidateUiState.UNAVAILABLE, model.candidateState)
        assertTrue(model.statusMessage!!.contains("другого аккаунта"))
    }
    @Test fun unsentDraftCanBeClearedWithoutServerCandidate() {
        val model = ReferralClientPresentation.model(state(projection()).copy(referral =
            ReferralClientState(durable = ReferralState("install", draftCode = "Draft"))))
        assertTrue(model.canClear)
    }

    @Test fun keyedUnknownShowsRetryAndRejectedReceiptNeverShowsConfirmed() {
        val registration = ReferralRegistrationSnapshot("registration-key-0001", "candidate")
        val durable = ReferralState("install", candidate = ReferralCandidate("candidate", "Invite"), registration = registration)
        val model = ReferralClientPresentation.model(ViewState(referral = ReferralClientState(durable)))
        assertTrue(model.canRetry)
        assertFalse(model.canClear)
        assertFalse(model.canEdit)
        assertEquals(ReferralCandidateUiState.LOCKED, model.candidateState)
        val rejected = ReferralAttributionReceipt("receipt", "account-a", "candidate", "reg", "registration-key-0001", "rejected", "self")
        val terminal = ReferralClientPresentation.model(ViewState(referral = ReferralClientState(
            durable.copy(candidate = null, receipt = rejected), verifiedRegistrationAccount = "account-a")))
        assertEquals(ReferralCandidateUiState.REJECTED, terminal.candidateState)
        assertFalse(terminal.canRetry)
        assertNull(terminal.ownCode)
    }
}

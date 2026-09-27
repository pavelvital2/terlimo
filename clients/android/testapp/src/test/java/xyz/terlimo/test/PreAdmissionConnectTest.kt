package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class PreAdmissionConnectTest {
    private fun source(path: String): String =
        listOf(File(path), File("testapp/$path"), File("../$path")).first { it.isFile }.readText()
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

    @Test
    fun `first connect is eligible only for none plus not_started`() {
        assertTrue(PreAdmissionConnect.eligible(state(projection())))
        assertTrue(PreAdmissionConnect.connectable(state(projection()), pendingChoice = false))
        assertFalse(PreAdmissionConnect.eligible(state(projection(dataAccess = "onboarding_hour"))))
        assertFalse(PreAdmissionConnect.eligible(state(projection(onboarding = "active"))))
        assertFalse(PreAdmissionConnect.eligible(state(projection(onboarding = "expired"))))
        assertFalse(PreAdmissionConnect.eligible(state(projection(dataAccess = "subscription_data"))))
        assertFalse(PreAdmissionConnect.eligible(state(null)))
    }

    @Test
    fun `management only me before forbidden catalog still offers explicit first connect`() {
        val waiting = state(projection(managementOnly = true)).copy(phase = "BootstrapConnecting")
        assertTrue(PreAdmissionConnect.connectable(waiting, pendingChoice = false))
        assertFalse(PreAdmissionConnect.connectable(waiting.copy(phase = "Connected"), pendingChoice = false))
        assertFalse(ExplicitConnectGate.arms(ExplicitConnectGate.Entry.RESUME))
        assertFalse(ExplicitConnectGate.arms(ExplicitConnectGate.Entry.BACKGROUND))
        assertTrue(ExplicitConnectGate.arms(ExplicitConnectGate.Entry.PRE_ADMISSION))
    }

    @Test
    fun `prohibited account binding and entitlement states never enter first connect`() {
        assertFalse(PreAdmissionConnect.eligible(state(projection(accountState = "REVOKED_SESSION"))))
        assertFalse(PreAdmissionConnect.eligible(state(projection(accountState = "EXPIRED"))))
        assertFalse(PreAdmissionConnect.eligible(state(projection(bindingStatus = "revoked"))))
        assertFalse(PreAdmissionConnect.eligible(state(projection(bindingStatus = "deactivated"))))
        assertFalse(PreAdmissionConnect.eligible(state(projection(entitlementStatus = "revoked"))))
        assertFalse(PreAdmissionConnect.eligible(state(projection(entitlementStatus = "expired"))))
        assertFalse(PreAdmissionConnect.eligible(state(projection(entitlementStatus = "unknown_review"))))
    }

    @Test
    fun `a pending selection or user choice blocks the pre-admission connect`() {
        val pendingNode = state(projection()).copy(pendingNodeId = "gw-0")
        assertTrue(PreAdmissionConnect.eligible(pendingNode))
        assertFalse(PreAdmissionConnect.connectable(pendingNode, pendingChoice = false))
        assertFalse(PreAdmissionConnect.connectable(state(projection()), pendingChoice = true))
    }

    @Test
    fun `background resume import and recovery never arm the explicit gate`() {
        val nonExplicit = listOf(
            ExplicitConnectGate.Entry.RESUME, ExplicitConnectGate.Entry.IMPORT,
            ExplicitConnectGate.Entry.BACKGROUND, ExplicitConnectGate.Entry.RECOVERY,
            ExplicitConnectGate.Entry.WAKE, ExplicitConnectGate.Entry.SWITCH,
            ExplicitConnectGate.Entry.PROBE,
        )
        for (entry in nonExplicit) assertFalse(ExplicitConnectGate.arms(entry))
        assertTrue(ExplicitConnectGate.arms(ExplicitConnectGate.Entry.PRE_ADMISSION))
        assertTrue(ExplicitConnectGate.arms(ExplicitConnectGate.Entry.SELECT))
        assertTrue(ExplicitConnectGate.arms(ExplicitConnectGate.Entry.ONE_TAP_CONNECT))
    }

    @Test
    fun `pre-admission arming is fenced to the active attempt`() {
        val arming = ExplicitConnectArming()
        assertFalse(arming.arm(ExplicitConnectGate.Entry.PRE_ADMISSION, null))
        assertFalse(arming.arm(ExplicitConnectGate.Entry.RESUME, "attempt-1"))
        assertTrue(arming.arm(ExplicitConnectGate.Entry.PRE_ADMISSION, "attempt-1"))
        assertTrue(arming.armed("attempt-1"))
        assertFalse(arming.armed("attempt-2"))
        arming.clear()
        assertFalse(arming.armed("attempt-1"))
    }

    @Test
    fun `owner hour purpose and warning are the exact texts on the existing surfaces`() {
        assertEquals(
            "1 час доступа для регистрации. После регистрации вы сможете выбрать пробный доступ на 7 дней или купить подписку",
            PreAdmissionConnect.HOUR_PURPOSE)
        assertEquals(
            "У вас будет 1 час для регистрации. Если не успеете зарегистрироваться, пробные 7 дней будут недоступны. " +
                "Продолжить можно будет после покупки подписки; регистрация останется обязательной",
            PreAdmissionConnect.HOUR_WARNING)

        // The purpose is stated on the pre-admission action surface, the warning on the
        // main status surface, both before the first hour can start.
        val orbit = source("src/main/java/xyz/terlimo/test/OrbitHomeHeader.kt")
        assertTrue(orbit.contains("preAdmissionConnect -> PreAdmissionConnect.HOUR_PURPOSE"))
        val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
        assertTrue(activity.contains("PreAdmissionConnect.HOUR_WARNING.takeIf { PreAdmissionConnect.eligible(state) }"))
        assertTrue(activity.substringAfter("private fun requestPreAdmissionConsent(")
            .contains("status.text = PreAdmissionConnect.HOUR_WARNING"))

        // The wording is display-only: the gate stays I/O-free and no payment/eligibility
        // code path is added.
        val gate = source("src/main/java/xyz/terlimo/test/PreAdmissionConnect.kt")
        listOf("http", "URL", "Service", "startActivity", "startService", "Billing", "billing", "checkout", "Checkout")
            .forEach { assertFalse(it, gate.contains(it)) }
    }
}

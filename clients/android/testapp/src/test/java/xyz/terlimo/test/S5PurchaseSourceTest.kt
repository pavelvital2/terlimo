package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Source pins for the S5 host purchase surface: frozen vocabulary only, durable
 * idempotency keys before the first send, no grant/entitlement write from any payment
 * result or redirect, and the purchase UI on the existing "Подписка" tab only.
 */
class S5PurchaseSourceTest {
    private fun source(path: String): String = listOf(File(path), File("testapp/$path"))
        .first { it.isFile }.readText()

    private val service by lazy { source("src/main/java/xyz/terlimo/test/SessionService.kt") }
    private val activity by lazy { source("src/main/java/xyz/terlimo/test/MainActivity.kt") }
    private val purchaseFlow by lazy { source("src/main/java/xyz/terlimo/test/PurchaseFlow.kt") }
    private val paymentsText by lazy { source("src/main/java/xyz/terlimo/test/PaymentsText.kt") }
    private val attemptsSource by lazy { source("src/main/java/xyz/terlimo/test/PurchaseAttempts.kt") }
    private val storeSource by lazy { source("src/main/java/xyz/terlimo/test/InstallationStore.kt") }

    private val purchaseActions: String by lazy {
        service.substringAfter("\"purchase_plans\" ->").substringBefore("\"cancel\" ->")
    }
    private val sender: String by lazy {
        service.substringAfter("private fun sendPurchaseOperation(")
            .substringBefore("private fun handlePurchaseEvent(")
    }
    private val eventHandler: String by lazy {
        service.substringAfter("private fun handlePurchaseEvent(")
            .substringBefore("private fun armPurchaseConfirmationWindow(")
    }
    private val accountBranch: String by lazy {
        service.substringAfter("\"account_access\" ->").substringBefore("\"telegram_registration\" ->")
    }
    private val purchaseSection: String by lazy {
        activity.substringAfter("// S5 purchase/renewal lives on this existing")
            .substringBefore("Маршрутизация приложений")
    }

    @Test
    fun hostSendsOnlyTheFrozenPaymentActions() {
        assertTrue(sender.contains("PaymentsContract.ACTION_PLANS_LIST"))
        assertTrue(sender.contains("PaymentsContract.ACTION_QUOTE_CREATE"))
        assertTrue(sender.contains("PaymentsContract.ACTION_PAYMENT_CREATE"))
        assertTrue(sender.contains("PaymentsContract.ACTION_PAYMENT_GET"))
        listOf("checkout_session", "checkout-session", "qr", "qr_").forEach {
            assertFalse(sender.lowercase().contains(it))
        }
        assertTrue(purchaseActions.contains("PurchaseOperation.Quote"))
        assertTrue(purchaseActions.contains("PurchaseOperation.Payment"))
    }

    @Test
    fun resultsAreParsedStrictlyAndMalformedEventsDoNotTearDownTheAttempt() {
        assertTrue(eventHandler.contains("PaymentsContract.parse(event)"))
        assertTrue(eventHandler.contains("runCatching"))
        assertTrue(eventHandler.contains("\"rejected\""))
    }

    @Test
    fun keysArePersistedBeforeTheSendAndReusedOnRetry() {
        assertTrue(storeSource.contains("purchase_attempt_state"))
        assertTrue(storeSource.contains("fun readPurchaseAttempt()"))
        assertTrue(storeSource.contains("fun writePurchaseAttempt("))
        assertTrue(storeSource.contains("class InstallationPurchaseAttemptStore"))
        assertTrue(attemptsSource.contains("interface PurchaseAttemptStore"))
        assertTrue(service.contains("PurchaseAttempts(InstallationPurchaseAttemptStore(storage))"))
        assertTrue(sender.contains("purchaseAttempts.beginQuote("))
        assertTrue(sender.contains("purchaseAttempts.beginPayment("))
        assertTrue(sender.contains(".put(\"idempotency_key\", record.quoteKey)"))
        assertTrue(sender.contains(".put(\"idempotency_key\", record.paymentKey)"))
        // The sender never mints a key itself: generation stays in the durable policy.
        assertFalse(sender.contains("UUID.randomUUID()"))
    }

    @Test
    fun noPaymentResultOrRedirectWritesEntitlementOrAccess() {
        assertFalse(eventHandler.contains("accountAccess ="))
        assertFalse(eventHandler.contains("writeAccountAccessState"))
        assertFalse(eventHandler.contains("grant ="))
        // A paid payment only triggers the existing fresh /me path and stays pending.
        assertTrue(eventHandler.contains("refresh_telegram_registration"))
        assertTrue(eventHandler.contains("armPurchaseConfirmationWindow(attempt)"))
        // The redirect/incoming-intent path never touches the purchase flow.
        val incoming = activity.substringAfter("override fun onNewIntent(")
            .substringBefore("override fun onSaveInstanceState(")
        assertFalse(activity.contains("handleIncomingIntent"))
        assertFalse(incoming.contains("purchase"))
        assertFalse(incoming.contains("payment"))
    }

    @Test
    fun confirmationComesOnlyFromTheAcceptedMeProjection() {
        assertTrue(accountBranch.contains("PurchaseFlow.onFreshMe("))
        // The confirmed purchase releases its own cold attempt through a dedicated
        // non-rights helper; the rights branch itself never stops the attempt.
        assertTrue(accountBranch.contains("releaseConfirmedColdPurchase(attempt)"))
        assertFalse(accountBranch.contains("stopAttempt("))
        val release = service.substringAfter("private fun releaseConfirmedColdPurchase(")
            .substringBefore("private fun sign(")
        assertTrue(release.contains("purchaseAttempts.restart()"))
        assertTrue(release.contains("purchaseGate.stopCold(attempt)"))
        assertTrue(purchaseFlow.contains("projection.entitlement.status != \"active\""))
        assertTrue(purchaseFlow.contains("projection.entitlement.type != \"paid\""))
        assertTrue(purchaseFlow.contains("projection.entitlement.revision != creditedRevision"))
        assertFalse(purchaseFlow.contains("entitlement ="))
        assertFalse(purchaseFlow.contains("write"))
    }

    @Test
    fun purchaseUiStaysOnTheExistingSubscriptionTabWithoutQrOrHostedCheckout() {
        assertEquals(4, activity.split("destination(\"").size - 1)
        assertTrue(activity.contains("subscriptionPanel.addView(subscriptionTerm)"))
        assertTrue(purchaseSection.contains("subscriptionPanel.addView(purchasePlansButton)"))
        assertTrue(activity.contains("PaymentsText.planLine"))
        assertTrue(activity.contains("PaymentsText.quoteLine"))
        assertTrue(activity.contains("PaymentsText.purchaseStatus"))
        assertTrue(activity.contains("SubscriptionTermText.term"))
        assertTrue(activity.contains("java.time.ZoneId.systemDefault()"))
        val purchaseRender = activity.substringAfter("private fun renderPurchase(")
            .substringBefore("private fun pendingSelectionStatus(")
        assertFalse(purchaseRender.contains("IntentIntegrator"))
        assertFalse(purchaseRender.contains("checkout_session"))
        assertFalse(purchaseRender.contains("checkout-session"))
        assertFalse(purchaseSection.contains("QR"))
        assertFalse(purchaseSection.contains("checkout"))
        assertTrue(paymentsText.contains("const val hostedCheckoutOffered = false"))
    }

    @Test
    fun aStaleQuoteIsNeverSilentlyReused() {
        assertTrue(purchaseActions.contains("PurchaseFlow.quoteExpired(current, java.time.Instant.now())"))
        assertTrue(purchaseActions.contains("purchaseAttempts.restart()"))
        assertTrue(purchaseActions.contains("PurchaseFlow.quoteExpiredState(current)"))
        assertTrue(purchaseFlow.contains("quote = null"))
    }
}

package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * §3.2B correction: the browser opens only as the consequence of an explicit user action
 * («Оплатить» or «Продолжить оплату»). The «Оплатить» marker is satisfied ONLY by the
 * correlated [PaymentCreateAck] of the exact sent quote and only once per create generation;
 * an ordinary status refresh, polling render, stale order or Activity recreation never opens
 * it, terminal statuses never open, and a failed launch stays visible and retryable through
 * the explicit continue action.
 */
class CheckoutOpenPolicyTest {
    private fun payment(id: String, reference: String?, status: String = "created") =
        PaymentStatusView(
            paymentId = id, paymentStatus = status, checkoutReference = reference,
            creditedEntitlementRevision = null, accessApplicationState = "not_requested",
        )

    private fun ack(quoteId: String, seq: Long, payment: PaymentStatusView) =
        PaymentCreateAck(quoteId = quoteId, seq = seq, payment = payment)

    private fun oversizedReference(): String {
        val prefix = "https://pay.example/"
        return prefix + "a".repeat(257 - prefix.length)
    }

    @Test
    fun explicitPayTapOpensOnceAndExplicitContinueReopensTheSameSavedPayment() {
        val policy = CheckoutOpenPolicy()
        val created = payment("pay-1", "https://pay.example/s/1")
        policy.onPayRequested("quote-1")
        val first = policy.autoOpenAfterPay(ack("quote-1", 1, created))
        assertNotNull(first)
        assertEquals("pay-1", first!!.paymentId)
        assertEquals("https://pay.example/s/1", first.url)
        policy.onOpened(first.paymentId)

        // The user closed the browser unpaid and explicitly continues: the SAME saved payment
        // is reopened (the policy has no service action and can never create a new order).
        assertTrue(policy.canContinue(created))
        val again = policy.onContinueRequested(created)
        assertNotNull(again)
        assertEquals("pay-1", again!!.paymentId)
        assertEquals("https://pay.example/s/1", again.url)

        // A repeated render never reopens (the generation was consumed), and a repeated
        // explicit «Оплатить» resend reuses the same order/payment id: the opened payment is
        // not auto-opened a second time even under a fresh create generation.
        assertNull(policy.autoOpenAfterPay(ack("quote-1", 1, created)))
        assertNull(policy.openError)
        policy.onPayRequested("quote-1")
        assertNull(policy.autoOpenAfterPay(ack("quote-1", 1, created)))
        assertNull(policy.autoOpenAfterPay(ack("quote-1", 2, created)))
    }

    @Test
    fun onlyCreatedAndPendingCanOpenAndTerminalStatusesNeverDo() {
        val terminals = listOf("paid", "failed", "expired", "refunded", "disputed")
        for (status in terminals) {
            val auto = CheckoutOpenPolicy()
            auto.onPayRequested("quote-1")
            assertNull(
                "auto $status",
                auto.autoOpenAfterPay(
                    ack("quote-1", 1, payment("pay-1", "https://pay.example/s/1", status))),
            )
            val continued = CheckoutOpenPolicy()
            assertNull(
                "continue $status",
                continued.onContinueRequested(payment("pay-1", "https://pay.example/s/1", status)),
            )
            assertFalse(
                "canContinue $status",
                continued.canContinue(payment("pay-1", "https://pay.example/s/1", status)),
            )
        }
        for (status in listOf("created", "pending")) {
            val auto = CheckoutOpenPolicy()
            auto.onPayRequested("quote-1")
            assertNotNull(
                "auto $status",
                auto.autoOpenAfterPay(
                    ack("quote-1", 1, payment("pay-1", "https://pay.example/s/1", status))),
            )
            val continued = CheckoutOpenPolicy()
            assertNotNull(
                "continue $status",
                continued.onContinueRequested(payment("pay-1", "https://pay.example/s/1", status)),
            )
            assertTrue(
                "canContinue $status",
                continued.canContinue(payment("pay-1", "https://pay.example/s/1", status)),
            )
        }
    }

    @Test
    fun failedCreateAttemptClearsTheMarkerSoALaterPaymentIsNotAutoOpened() {
        val policy = CheckoutOpenPolicy()
        policy.onPayRequested("quote-1")
        policy.onPayFailed()
        // The create failed: neither the same payment nor a later unrelated one auto-opens.
        assertNull(policy.autoOpenAfterPay(ack("quote-1", 1, payment("pay-1", "https://pay.example/s/1"))))
        assertNull(
            policy.autoOpenAfterPay(ack("quote-9", 2, payment("pay-9", "https://pay.example/s/9", "pending"))),
        )
        // Only the explicit continue action may open an already-created saved payment.
        assertNotNull(policy.onContinueRequested(payment("pay-1", "https://pay.example/s/1")))

        // An attempt replacement (clearAwaiting) has the same effect.
        val replaced = CheckoutOpenPolicy()
        replaced.onPayRequested("quote-1")
        replaced.clearAwaiting()
        assertNull(replaced.autoOpenAfterPay(ack("quote-1", 1, payment("pay-1", "https://pay.example/s/1"))))
    }

    @Test
    fun staleAcknowledgementNeverConsumesTheMarkerOrStealsTheOpen() {
        val policy = CheckoutOpenPolicy()
        policy.onPayRequested("quote-1")
        val old = payment("pay-old", "https://pay.example/s/old")
        val fresh = payment("pay-fresh", "https://pay.example/s/fresh")
        // An ack of a replaced attempt opens nothing...
        assertNull(policy.autoOpenAfterPay(ack("quote-2", 7, old)))
        // ...and the mismatched ack did not consume the marker: the armed attempt still
        // auto-opens exactly once for its own correlated create result.
        val open = policy.autoOpenAfterPay(ack("quote-1", 8, fresh))
        assertNotNull(open)
        assertEquals("pay-fresh", open!!.paymentId)
        assertNull(policy.autoOpenAfterPay(ack("quote-1", 8, fresh)))

        // A new explicit tap replaces the old marker with the new sent quote id.
        val replaced = CheckoutOpenPolicy()
        replaced.onPayRequested("quote-1")
        replaced.onPayRequested("quote-2")
        assertNull(replaced.autoOpenAfterPay(ack("quote-1", 1, old)))
        assertNotNull(replaced.autoOpenAfterPay(ack("quote-2", 2, old)))
    }

    @Test
    fun ordinaryRenderAndRecreationNeverAutoOpen() {
        val policy = CheckoutOpenPolicy()
        val polled = payment("pay-1", "https://pay.example/s/1")
        // A payment from a server status refresh/poll with no explicit tap opens nothing, and
        // even an ack-shaped signal alone can never open without the armed marker.
        assertNull(policy.autoOpenAfterPay(null))
        assertNull(policy.autoOpenAfterPay(ack("quote-1", 1, polled)))
        assertNull(policy.openError)

        // While armed, a render with no create result yet keeps the marker; a stale ack of
        // another attempt is ignored and the awaited ack then opens once.
        policy.onPayRequested("quote-1")
        assertNull(policy.autoOpenAfterPay(null))
        assertNull(policy.autoOpenAfterPay(ack("quote-9", 2, polled)))
        assertNotNull(policy.autoOpenAfterPay(ack("quote-1", 3, polled)))

        // A marker armed before recreation is deliberately never restored.
        val armed = CheckoutOpenPolicy()
        armed.onPayRequested("quote-1")
        val restoredArmed = CheckoutOpenPolicy()
        restoredArmed.restore(armed.savedState())
        assertNull(restoredArmed.autoOpenAfterPay(ack("quote-1", 1, polled)))
        assertNull(restoredArmed.openError)
    }

    @Test
    fun failedLaunchIsVisibleRetryableAndNeverAutoRetried() {
        val policy = CheckoutOpenPolicy()
        val created = payment("pay-1", "https://pay.example/s/1")
        policy.onPayRequested("quote-1")
        val open = policy.autoOpenAfterPay(ack("quote-1", 1, created))
        assertNotNull(open)
        policy.onOpenFailed(PaymentsText.CHECKOUT_NO_BROWSER_TEXT)
        assertEquals(PaymentsText.CHECKOUT_NO_BROWSER_TEXT, policy.openError)
        // The failed launch is not retried automatically (its generation was consumed) while
        // the explicit continue action stays available and works once.
        assertNull(policy.autoOpenAfterPay(ack("quote-1", 1, created)))
        assertEquals(PaymentsText.CHECKOUT_NO_BROWSER_TEXT, policy.openError)
        assertTrue(policy.canContinue(created))
        val retry = policy.onContinueRequested(created)
        assertNotNull(retry)
        policy.onOpened(retry!!.paymentId)
        assertNull(policy.openError)
    }

    @Test
    fun invalidOrOversizedReferenceNeverOpensAndKeepsTheOrder() {
        val policy = CheckoutOpenPolicy()
        policy.onPayRequested("quote-2")
        assertNull(policy.autoOpenAfterPay(ack("quote-2", 1, payment("pay-2", null))))
        assertEquals(PaymentsText.CHECKOUT_INVALID_LINK_TEXT, policy.openError)
        assertFalse(policy.canContinue(payment("pay-2", "http://pay.example/s/2")))
        assertNull(policy.onContinueRequested(payment("pay-2", "http://pay.example/s/2")))
        // The payment is kept: once the provider returns a valid URL for the same order id,
        // the explicit continue opens it.
        assertNotNull(policy.onContinueRequested(payment("pay-2", "https://pay.example/s/2")))
        assertFalse(policy.canContinue(payment("pay-2", oversizedReference())))

        val oversized = CheckoutOpenPolicy()
        oversized.onPayRequested("quote-3")
        assertNull(oversized.autoOpenAfterPay(ack("quote-3", 1, payment("pay-3", oversizedReference()))))
        assertEquals(PaymentsText.CHECKOUT_INVALID_LINK_TEXT, oversized.openError)
        assertFalse(oversized.canContinue(payment("pay-3", oversizedReference())))
    }

    @Test
    fun restoredStateAfterRecreationNeverAutoOpens() {
        val policy = CheckoutOpenPolicy()
        val created = payment("pay-1", "https://pay.example/s/1")
        policy.onPayRequested("quote-1")
        val open = policy.autoOpenAfterPay(ack("quote-1", 11, created))
        assertNotNull(open)
        policy.onOpened(open!!.paymentId)
        val saved = policy.savedState()
        assertEquals("pay-1", saved.openedPaymentId)
        assertEquals(11L, saved.lastConsumedAckSeq)

        val recreated = CheckoutOpenPolicy()
        recreated.restore(saved)
        assertEquals("pay-1", recreated.savedState().openedPaymentId)
        assertEquals(11L, recreated.savedState().lastConsumedAckSeq)
        // Without an explicit tap after recreation nothing opens.
        assertNull(recreated.autoOpenAfterPay(ack("quote-1", 11, created)))
        assertNull(recreated.autoOpenAfterPay(ack("quote-2", 12, payment("pay-2", "https://pay.example/s/2"))))
        // A new explicit tap after recreation can never re-consume the persisted generation...
        recreated.onPayRequested("quote-1")
        assertNull(recreated.autoOpenAfterPay(ack("quote-1", 11, created)))
        // ...only a new create generation of the sent quote opens again.
        val reopened = recreated.autoOpenAfterPay(ack("quote-1", 12, payment("pay-2", "https://pay.example/s/2")))
        assertNotNull(reopened)
        assertEquals("pay-2", reopened!!.paymentId)
        // The opened payment is never auto-opened again, but a deliberate continue may always
        // reopen the same saved payment.
        assertTrue(recreated.canContinue(created))
        assertNotNull(recreated.onContinueRequested(created))

        // The visible error survives recreation but still opens nothing by itself.
        val failed = CheckoutOpenPolicy()
        failed.onOpenFailed(PaymentsText.CHECKOUT_NO_BROWSER_TEXT)
        val restoredError = CheckoutOpenPolicy()
        restoredError.restore(failed.savedState())
        assertEquals(PaymentsText.CHECKOUT_NO_BROWSER_TEXT, restoredError.openError)
        assertNull(restoredError.autoOpenAfterPay(ack("quote-1", 1, created)))
        assertEquals(PaymentsText.CHECKOUT_NO_BROWSER_TEXT, restoredError.openError)
    }

    @Test
    fun renderPurchaseHasNoUnconditionalDecideAndThePayTapArmsTheExplicitRequest() {
        val activity = listOf(File("src/main/java/xyz/terlimo/test/MainActivity.kt"),
            File("testapp/src/main/java/xyz/terlimo/test/MainActivity.kt"))
            .first { it.isFile }.readText()
        val renderPurchase = activity.substringAfter("private fun renderPurchase(")
            .substringBefore("private fun purchaseStatusLine(")
        assertFalse(renderPurchase.contains("CheckoutRedirect.decide("))
        assertTrue(renderPurchase.contains(
            "checkoutOpenPolicy.autoOpenAfterPay(state.purchase?.createAck)"))
        // No ViewState payment may ever be matched against the live marker again.
        assertFalse(renderPurchase.contains("autoOpenAfterPay(payment"))
        // While a purchase request is outstanding the surface shows waiting and disables
        // pay/plan/method; the service gate is mandatory independently of this UI.
        assertTrue(renderPurchase.contains("val sending = state.purchase?.sending == true"))
        assertTrue(renderPurchase.contains("purchasePayButton.isEnabled = !sending"))
        assertTrue(renderPurchase.contains("purchasePlanButton.isEnabled = !sending"))
        assertTrue(renderPurchase.contains("purchaseMethodButton.isEnabled = !sending"))
        assertFalse(activity.contains("purchaseQuoteButton"))
        assertFalse(renderPurchase.contains("setAction(\"purchase_quote\")"))
        val selection = activity.substringAfter("private fun showPurchaseMethods(")
            .substringBefore("private fun renderPurchase(")
        assertTrue(selection.contains("setAction(\"purchase_quote\")"))
        assertFalse(selection.contains("setAction(\"purchase_pay\")"))
        val cancellation = activity.substringAfter("private fun clearPurchaseSelection(")
            .substringBefore("private fun showPurchasePlans(")
        assertFalse(cancellation.contains("startService("))
        assertFalse(cancellation.contains("startForegroundService("))
        val service = listOf(File("src/main/java/xyz/terlimo/test/SessionService.kt"),
            File("testapp/src/main/java/xyz/terlimo/test/SessionService.kt")).first { it.isFile }.readText()
        val quoteAction = service.substringAfter("\"purchase_quote\" ->")
            .substringBefore("\"purchase_pay\" ->")
        // A second queued selection cannot replace the owner while the first cold quote starts.
        assertTrue(quoteAction.contains("purchaseFlight.busy() || view.purchase?.sending == true"))
        assertTrue(quoteAction.contains("quote = null, createAck = null, sending = true"))

        assertTrue(renderPurchase.contains("checkoutOpenPolicy.clearAwaiting()"))
        assertTrue(renderPurchase.contains("checkoutOpenPolicy.canContinue(payment)"))
        val payListener = activity.substringAfter("purchasePayButton = Button(this)")
            .substringBefore("subscriptionPanel.addView(purchasePayButton)")
        assertTrue(payListener.contains("noteExplicitPay(quote.quoteId)"))
        val notePay = activity.substringAfter("private fun noteExplicitPay(")
            .substringBefore("private fun continueExistingPayment(")
        assertTrue(notePay.contains("checkoutOpenPolicy.onPayRequested(sentQuoteId)"))
        val continueAction = activity.substringAfter("private fun continueExistingPayment(")
            .substringBefore("private fun launchCheckoutBrowser(")
        assertTrue(continueAction.contains("checkoutOpenPolicy.onContinueRequested(payment)"))
    }
}

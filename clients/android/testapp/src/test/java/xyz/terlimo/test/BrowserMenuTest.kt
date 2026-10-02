package xyz.terlimo.test

import java.time.Instant
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class BrowserMenuTest {
    private val now = Instant.parse("2026-10-02T11:00:00Z")
    private val plan = PaymentPlan("p30", "TERLIMO — 30 дней", "days:30", 2, 20_000, "RUB",
        listOf("crypto", "card", "sbp"))
    private val quote = PaymentQuote("q1", 20_000, "RUB", "days:30", 2, "sbp", "2026-10-02T11:05:00Z")

    @Test
    fun labelsAndOrderMatchProductionExportWhileAmountsRemainServerOwned() {
        val export = JSONObject(javaClass.getResource("/browser-production-menu-20261002.json")!!.readText())
        val plans = listOf(plan.copy(planId = "p6", durationCode = "months:6", amountMinor = 84_000),
            plan, plan.copy(planId = "p3", durationCode = "months:3", amountMinor = 48_000))
        fun rows(name: String): List<String> = export.getJSONArray(name).let { array ->
            (0 until array.length()).map(array::getString)
        }
        assertEquals(rows("period_rows"), PaymentsText.orderedPlans(plans)
            .map(PaymentsText::purchasePlanLine) + PaymentsText.BACK_TEXT)
        assertEquals(rows("payment_rows"), PaymentsText.orderedMethods(plan)
            .map(PaymentsText::methodLabel) + PaymentsText.CANCEL_TEXT)
        assertEquals(export.getString("pay_button"), PaymentsText.PAY_TEXT)
        assertEquals("days:30", PaymentsText.orderedPlans(plans).first().durationCode)
        assertEquals("1 мес. - 217.53 RUB", PaymentsText.purchasePlanLine(plan.copy(amountMinor = 21_753)))
        assertEquals(listOf("card"), PaymentsText.orderedMethods(plan.copy(methods = listOf("card"))))
    }

    @Test
    fun changedSelectionOrStalePriceCannotPayAndTheExistingOrderCanStillContinue() {
        val order = PaymentStatusView("existing", "pending", "https://example.test/order", null, "not_requested")
        val initial = PurchaseState(phase = PurchaseFlow.QUOTE_READY, plans = listOf(plan),
            selectedPlanId = plan.planId, selectedMethod = "sbp", quote = quote, payment = order)
        assertEquals(quote, PurchaseFlow.payableQuote(initial, plan, "sbp", now))
        // The Activity's explicit new method is rejected even before the service consumes it.
        assertNull(PurchaseFlow.payableQuote(initial, plan, "card", now))
        val changed = PurchaseFlow.selectMethod(initial, "card")
        val late = PurchaseFlow.quoteReady(changed, quote)
        assertNull(PurchaseFlow.payableQuote(late, plan, "card", now))
        assertNull(PurchaseFlow.payableQuote(initial, plan.copy(planId = "other"), "sbp", now))
        assertNull(PurchaseFlow.payableQuote(initial.copy(quote = quote.copy(amountMinor = 19_999)), plan, "sbp", now))
        assertNull(PurchaseFlow.payableQuote(initial.copy(quote = quote.copy(currency = "USD")), plan, "sbp", now))
        assertNull(PurchaseFlow.payableQuote(initial, plan, "sbp", Instant.parse(quote.expiresAt)))
        val current = PurchaseFlow.quoteReady(changed, quote.copy(quoteId = "q2", method = "card"))
        assertEquals("q2", PurchaseFlow.payableQuote(current, plan, "card", now)?.quoteId)
        assertEquals(order, current.payment)
        val browser = CheckoutOpenPolicy()
        assertEquals("existing", browser.onContinueRequested(current.payment)?.paymentId)
        browser.clearAwaiting() // Back/Cancel disarm browser creation, not the pending order.
        assertEquals("existing", browser.onContinueRequested(current.payment)?.paymentId)
        assertNull(current.createAck)
        val paid = PurchaseFlow.paymentResult(current, order.copy(paymentStatus = "paid"))
        assertNull(PurchaseFlow.payableQuote(paid, plan, "card", now))
        assertFalse(PurchaseVisibility.payVisible(current.quote, PurchaseFlow.paidAwaitingBinding(paid)))
    }
}

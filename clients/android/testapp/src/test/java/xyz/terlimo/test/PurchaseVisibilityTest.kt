package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Host visibility of the purchase controls, exactly as renderPurchase applies it. It covers the
 * ordinary matrix (no quote → only the quote request; quote → line + pay) and the parked paid
 * order (S5 §3.2C), where the quote control, the quote line and the pay control are all hidden
 * so a second payment is never offered. This is the check that catches an inverted condition.
 */
class PurchaseVisibilityTest {
    private val quote = PaymentQuote("q-1", 30_000, "RUB", "days:30", 2, "sbp", "2026-09-30T15:35:00Z")

    @Test
    fun noQuoteOffersOnlyTheQuoteRequest() {
        assertTrue(PurchaseVisibility.quoteButtonVisible(null, false))
        assertFalse(PurchaseVisibility.payVisible(null, false))
        assertFalse(PurchaseVisibility.quoteLineVisible(null, false))
    }

    @Test
    fun acceptedQuoteShowsTheLineAndPayButNotTheQuoteRequest() {
        assertFalse(PurchaseVisibility.quoteButtonVisible(quote, false))
        assertTrue(PurchaseVisibility.payVisible(quote, false))
        assertTrue(PurchaseVisibility.quoteLineVisible(quote, false))
    }

    @Test
    fun parkedPaidOrderHidesTheQuoteRequestTheLineAndPay() {
        assertFalse(PurchaseVisibility.quoteButtonVisible(null, true))
        assertFalse(PurchaseVisibility.quoteButtonVisible(quote, true))
        assertFalse(PurchaseVisibility.payVisible(quote, true))
        assertFalse(PurchaseVisibility.quoteLineVisible(quote, true))
    }

    @Test
    fun renderPurchaseUsesTheSharedVisibilityPolicy() {
        val activity = listOf(
            File("src/main/java/xyz/terlimo/test/MainActivity.kt"),
            File("testapp/src/main/java/xyz/terlimo/test/MainActivity.kt"),
        ).first { it.isFile }.readText()
        val render = activity.substringAfter("private fun renderPurchase(")
            .substringBefore("private fun purchaseStatusLine(")
        assertTrue(render.contains("PurchaseVisibility.quoteLineVisible(quote, bindingPaid)"))
        assertTrue(render.contains("PurchaseVisibility.quoteButtonVisible(quote, bindingPaid)"))
        assertTrue(render.contains("PurchaseVisibility.payVisible(quote, bindingPaid)"))
    }
}

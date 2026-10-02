package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** Confirmed price/Pay remain gated, especially for an existing paid order awaiting binding. */
class PurchaseVisibilityTest {
    private val quote = PaymentQuote("q-1", 30_000, "RUB", "days:30", 2, "sbp", "2026-09-30T15:35:00Z")

    @Test
    fun noQuoteCannotOfferPay() {
        assertFalse(PurchaseVisibility.payVisible(null, false))
        assertFalse(PurchaseVisibility.quoteLineVisible(null, false))
    }

    @Test
    fun acceptedQuoteShowsTheLineAndPay() {
        assertTrue(PurchaseVisibility.payVisible(quote, false))
        assertTrue(PurchaseVisibility.quoteLineVisible(quote, false))
    }

    @Test
    fun parkedPaidOrderHidesTheLineAndPay() {
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
        assertTrue(render.contains("PurchaseVisibility.payVisible(quote, bindingPaid)"))
    }
}

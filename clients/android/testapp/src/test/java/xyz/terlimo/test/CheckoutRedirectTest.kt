package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class CheckoutRedirectTest {
    private fun payment(id: String, reference: String?, status: String = "created") =
        PaymentStatusView(
            paymentId = id, paymentStatus = status, checkoutReference = reference,
            creditedEntitlementRevision = null, accessApplicationState = "not_requested",
        )

    @Test
    fun onlyABoundedHttpsProviderUrlIsOpened() {
        assertEquals("https://pay.example/session/1", CheckoutRedirect.providerUrl("https://pay.example/session/1"))
        assertNull(CheckoutRedirect.providerUrl("http://pay.example/session/1"))
        assertNull(CheckoutRedirect.providerUrl("https://user:pass@pay.example/s/1"))
        assertNull(CheckoutRedirect.providerUrl("https:///no-host"))
        assertNull(CheckoutRedirect.providerUrl("  "))
        assertNull(CheckoutRedirect.providerUrl(null))
        assertNull(CheckoutRedirect.providerUrl("not a url"))
    }

    @Test
    fun theAcceptedReferenceBoundIsExactAt256() {
        val prefix = "https://pay.example/"
        val atBound = prefix + "a".repeat(256 - prefix.length)
        assertEquals(256, atBound.length)
        assertEquals(atBound, CheckoutRedirect.providerUrl(atBound))
        val overBound = prefix + "a".repeat(257 - prefix.length)
        assertEquals(257, overBound.length)
        assertNull(CheckoutRedirect.providerUrl(overBound))
    }

    @Test
    fun browserReturnNeverWritesPaidLocallyServerStatusOnly() {
        // A server "paid" status only awaits server confirmation, never CONFIRMED locally.
        val paid = PurchaseFlow.paymentResult(null, payment("pay-1", null, status = "paid"))
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION, paid.phase)
        // CONFIRMED is produced only by onFreshMe (a fresh accepted server /me), nowhere else.
        val src = listOf(java.io.File("src/main/java/xyz/terlimo/test/PurchaseFlow.kt"),
            java.io.File("testapp/src/main/java/xyz/terlimo/test/PurchaseFlow.kt"))
            .first { it.isFile }.readText()
        assertEquals(1, Regex("phase = CONFIRMED").findAll(src).count())
        assertEquals(0, Regex("paymentStatus == \"paid\" -> CONFIRMED").findAll(src).count())
    }
}

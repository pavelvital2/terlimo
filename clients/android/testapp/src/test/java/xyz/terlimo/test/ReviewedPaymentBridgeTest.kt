package xyz.terlimo.test

import org.json.JSONArray
import org.junit.Assert.*
import org.junit.Test

class ReviewedPaymentBridgeTest {
    @Test fun actualServerEnvelopesSurviveProductionBridgeAndStrictKotlinParser() {
        val text = checkNotNull(javaClass.getResourceAsStream("/payment-reviewed-bridge.json"))
            .bufferedReader().use { it.readText() }
        val captures = JSONArray(text)
        assertEquals(6, captures.length())
        for (index in 0 until captures.length()) {
            val capture = captures.getJSONObject(index)
            val scenario = capture.getString("scenario")
            val envelope = capture.getString("envelope")
            val event = PaymentsContract.parse(capture.getJSONObject("event"))
            when ("$scenario/$envelope") {
                "subscription/quote_envelope" -> {
                    val quote = (event as PaymentsEvent.Quote).quote
                    assertEquals("subscription", quote.product!!.kind)
                    assertEquals(2, quote.deviceLimit)
                    assertTrue(quote.product!!.renewExtraSlotIds.isEmpty())
                }
                "addon/quote_envelope" -> {
                    val product = (event as PaymentsEvent.Quote).quote.product!!
                    assertEquals("device_addon", product.kind)
                    assertEquals(1, product.deviceDelta)
                    assertEquals(product.targetValidUntil, product.validUntil)
                }
                "selected_renewal/plans_envelope" -> {
                    val plan = (event as PaymentsEvent.Plans).plans.first { it.planId == "terlimo-30d" }
                    assertEquals(2, plan.product!!.extraSlots.size)
                    assertTrue(plan.product!!.renewExtraSlotIds.isEmpty())
                }
                "selected_renewal/quote_envelope" -> {
                    val product = (event as PaymentsEvent.Quote).quote.product!!
                    assertEquals(2, product.extraSlots.size)
                    assertEquals(1, product.renewExtraSlotIds.size)
                    assertTrue(product.extraSlots.map { it.slotId }.containsAll(product.renewExtraSlotIds))
                    assertEquals(3, product.deviceLimit)
                }
                "selected_renewal/paid_envelope" -> {
                    val receipt = (event as PaymentsEvent.Payment).payment
                    assertEquals("paid", receipt.paymentStatus)
                    assertEquals("applied", receipt.creditState)
                    assertEquals(3, receipt.creditedProduct!!.deviceLimit)
                    assertEquals(4, receipt.creditedProduct!!.currentDeviceLimit)
                }
                "paid_needs_review/status_envelope" -> {
                    val receipt = (event as PaymentsEvent.Payment).payment
                    assertEquals("paid", receipt.paymentStatus)
                    assertEquals("needs_review", receipt.creditState)
                    assertEquals("target_expired", receipt.creditReviewReason)
                    assertNull(receipt.creditedProduct)
                    assertTrue(PurchaseFlow.paidAwaitingBinding(PurchaseFlow.paymentResult(null, receipt)))
                }
                else -> fail("Unexpected captured case")
            }
        }
    }
}

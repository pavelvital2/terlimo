package xyz.terlimo.test

import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Test

class PaymentProductContractTest {
    @Test
    fun addonPlanAndQuoteKeepTheServerPeriodAndHaveNoNewCapacityCap() {
        val product = addonProduct()
        val plan = (PaymentsContract.parse(plans(plan(product))) as PaymentsEvent.Plans).plans.single()
        val plannedProduct = plan.product!!
        assertEquals("until:$END", plan.durationCode)
        assertEquals(1, plannedProduct.deviceDelta)
        assertEquals(101, plannedProduct.deviceLimit)
        assertNull(plannedProduct.extraSlots.single().renewAmountMinor)
        val quote = (PaymentsContract.parse(quote(product)) as PaymentsEvent.Quote).quote
        assertEquals(101, quote.deviceLimit)
        assertEquals(plannedProduct, quote.product)
        assertEquals(500L, quote.amountMinor)
    }

    @Test
    fun subscriptionKeepsSelectedSlotIdsAndStructurallyIndependentSlotSnapshots() {
        val product = subscriptionProduct().put("base_amount_minor", 3_000_000_000L)
        val parsed = (PaymentsContract.parse(quote(product).put("duration_code", "days:30")
            .put("device_limit", 3)) as PaymentsEvent.Quote).quote.product!!
        assertEquals(listOf(SLOT), parsed.renewExtraSlotIds)
        assertEquals(3_000_000_000L, parsed.baseAmountMinor)
        assertEquals(700L, parsed.extraSlots.single().renewAmountMinor)
        // Reviewed quote.extra_slots includes all eligible slots, not only selected IDs.
        // Structural DTO parsing preserves that inventory without deriving it from selection.
        assertEquals(OTHER_SLOT, parsed.extraSlots.single().slotId)
    }

    @Test
    fun paidNeedsReviewAndActualCreditRemainDistinctFromTheQuotedProduct() {
        val reviewed = (PaymentsContract.parse(payment().put("product", addonProduct())
            .put("credit_state", "needs_review").put("credit_review_reason", "target_expired"))
            as PaymentsEvent.Payment).payment
        assertEquals("paid", reviewed.paymentStatus)
        assertEquals("needs_review", reviewed.creditState)
        assertEquals("target_expired", reviewed.creditReviewReason)
        assertNull(reviewed.creditedProduct)
        val receipt = JSONObject().put("valid_from", "2026-10-03T10:00:00Z")
            .put("valid_until", JSONObject.NULL).put("device_limit", 3)
            .put("current_device_limit", 105)
        val credited = (PaymentsContract.parse(payment().put("credit_state", "applied")
            .put("credited_product", receipt)) as PaymentsEvent.Payment).payment.creditedProduct!!
        assertEquals("2026-10-03T10:00:00Z", credited.validFrom)
        assertNull(credited.validUntil)
        assertEquals(105, credited.currentDeviceLimit)
    }

    @Test
    fun nullableV2ProductsAndLegacyOmissionsRemainCompatible() {
        assertNull((PaymentsContract.parse(quote().put("device_limit", 2)
            .put("duration_code", "days:30")) as PaymentsEvent.Quote).quote.product)
        assertNull((PaymentsContract.parse(plans(plan(JSONObject.NULL)
            .put("duration_code", "days:30"))) as PaymentsEvent.Plans).plans.single().product)
        val legacy = payment().apply {
            remove("product"); remove("credit_state"); remove("credit_review_reason"); remove("credited_product")
        }.put("payment_id", "legacy-payment-id")
        val status = (PaymentsContract.parse(legacy) as PaymentsEvent.Payment).payment
        assertNull(status.product)
        assertNull(status.creditState)
        assertNull(status.creditedProduct)
    }

    @Test
    fun v2PreservesEveryAcceptedLegacyPaymentAndAccessStatus() {
        for (status in PaymentsContract.PAYMENT_STATUSES) {
            for (access in PaymentsContract.ACCESS_APPLICATION_STATES) {
                val parsed = (PaymentsContract.parse(payment().put("payment_status", status)
                    .put("access_application_state", access)) as PaymentsEvent.Payment).payment
                assertEquals(status, parsed.paymentStatus)
                assertEquals(access, parsed.accessApplicationState)
            }
        }
    }

    @Test
    fun partialV2ShapesAndUnknownFieldsFailClosed() {
        for (key in listOf("product", "credit_state", "credit_review_reason", "credited_product")) {
            rejected(payment().apply { remove(key) })
        }
        rejected(payment().put("credit_state", JSONObject.NULL))
        rejected(payment().put("credit_state", "refunded"))
        rejected(payment().put("credit_review_reason", "new_reason"))
        rejected(quote(addonProduct().apply { remove("target_entitlement_id") }))
        rejected(quote(addonProduct().put("account_id", "hidden-owner")))
        rejected(quote().put("product", "not-an-object"))
        rejected(plans(plan(addonProduct())).put("product", JSONObject.NULL))
        val mixed = plans(plan(addonProduct())).apply {
            getJSONArray("plans").put(plan(JSONObject.NULL).apply {
                remove("product"); put("duration_code", "days:30")
            })
        }
        rejected(mixed)
    }

    @Test
    fun v2RejectsCoercedNumbersMalformedIdsDatesAndSlotShapes() {
        rejected(quote(addonProduct()).put("device_limit", "101"))
        rejected(quote(addonProduct()).put("device_limit", 101.5))
        rejected(quote(addonProduct()).put("device_limit", 3_000_000_000L))
        rejected(quote(addonProduct()).put("quote_id", "q-legacy"))
        rejected(quote(addonProduct()).put("expires_at", "2026-02-30T10:00:00Z"))
        rejected(quote(addonProduct()).put("duration_code", "until:2026-11-02T10:00:00+03:00"))
        rejected(quote(addonProduct().put("extra_amount_minor", "500")))
        rejected(quote(addonProduct().put("device_delta", 1.0)))
        rejected(quote(addonProduct().put("target_entitlement_id", "device-id")))
        rejected(quote(addonProduct().put("target_valid_until", JSONObject.NULL)))
        rejected(quote(addonProduct().put("renew_extra_slot_ids", JSONArray().put(SLOT))))
        rejected(quote(addonProduct().put("extra_slots", JSONArray().put(
            slot(SLOT, JSONObject.NULL).put("device_id", "must-not-be-a-slot")))))
        rejected(quote(addonProduct().put("extra_slots", JSONArray().put(slot(SLOT, 0)))))
        rejected(quote(subscriptionProduct().put("renew_extra_slot_ids", JSONArray()
            .put(SLOT).put(SLOT.uppercase()))).put("duration_code", "days:30").put("device_limit", 3))
        rejected(quote(addonProduct()).put("schema_version", "2.0"))
        rejected(payment().put("credited_product", JSONObject().put("valid_from", START)
            .put("valid_until", END).put("device_limit", 1).put("current_device_limit", 2)))
    }

    private fun rejected(event: JSONObject) {
        assertThrows(IllegalStateException::class.java) { PaymentsContract.parse(event) }
    }

    private fun envelope(type: String): JSONObject = JSONObject()
        .put("v", 1).put("attempt_id", "attempt").put("type", type).put("state", "ok")
        .put("request_id", "0123456789abcdef0123456789abcdef")
        .put("server_time", START).put("schema_version", "1.0")

    private fun money(): JSONObject = JSONObject().put("amount_minor", 500).put("currency", "RUB")

    private fun plan(product: Any): JSONObject = JSONObject().put("plan_id", "terlimo-extra-device")
        .put("title", "Дополнительное место").put("duration_code", "until:$END")
        .put("base_device_limit", 2).put("amount", money())
        .put("methods", JSONArray().put("sbp").put("card").put("crypto")).put("product", product)

    private fun plans(plan: JSONObject): JSONObject = envelope(PaymentsContract.TYPE_PLANS_LIST_RESULT)
        .put("plans_revision", "7").put("plans", JSONArray().put(plan))

    private fun quote(product: Any = JSONObject.NULL): JSONObject =
        envelope(PaymentsContract.TYPE_QUOTE_CREATE_RESULT).put("quote_id", ID)
            .put("amount", money()).put("duration_code", "until:$END")
            .put("device_limit", 101).put("method", "sbp")
            .put("expires_at", "2026-10-02T10:15:00Z").put("product", product)

    private fun payment(): JSONObject = envelope(PaymentsContract.TYPE_PAYMENT_GET_RESULT)
        .put("payment_id", ID).put("payment_status", "paid")
        .put("checkout_reference", JSONObject.NULL).put("credited_entitlement_revision", "8")
        .put("access_application_state", "applied").put("product", JSONObject.NULL)
        .put("credit_state", "unapplied").put("credit_review_reason", JSONObject.NULL)
        .put("credited_product", JSONObject.NULL)

    private fun addonProduct(): JSONObject = JSONObject().put("kind", "device_addon")
        .put("plan_id", "terlimo-extra-device").put("device_delta", 1)
        .put("target_entitlement_id", ID).put("target_valid_until", END)
        .put("valid_from", START).put("valid_until", END)
        .put("renew_extra_slot_ids", JSONArray()).put("base_amount_minor", 0)
        .put("extra_amount_minor", 500).put("device_limit", 101)
        .put("extra_slots", JSONArray().put(slot(SLOT, JSONObject.NULL)))

    private fun subscriptionProduct(): JSONObject = addonProduct().put("kind", "subscription")
        .put("plan_id", "terlimo-30d").put("device_delta", 0).put("device_limit", 3)
        .put("renew_extra_slot_ids", JSONArray().put(SLOT)).put("base_amount_minor", 20_000)
        .put("extra_slots", JSONArray().put(slot(OTHER_SLOT, 700)))

    private fun slot(id: String, renewal: Any): JSONObject = JSONObject()
        .put("slot_id", id).put("expires_at", END).put("renew_amount_minor", renewal)

    private companion object {
        const val ID = "01234567-89ab-cdef-0123-456789abcdef"
        const val SLOT = "11234567-89ab-cdef-0123-456789abcdef"
        const val OTHER_SLOT = "21234567-89ab-cdef-0123-456789abcdef"
        const val START = "2026-10-02T10:00:00Z"
        const val END = "2026-11-02T10:00:00Z"
    }
}

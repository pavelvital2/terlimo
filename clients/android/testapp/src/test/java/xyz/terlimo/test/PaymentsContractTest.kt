package xyz.terlimo.test

import java.time.ZoneId
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

class PaymentsContractTest {
    private val moscow: ZoneId = ZoneId.of("Europe/Moscow")

    private fun plansEvent(plans: String = PLAN_JSON): JSONObject = JSONObject(
        """
        {"v":1,"attempt_id":"attempt","type":"plans_list_result","state":"ok",
         "request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-19T15:20:00Z",
         "schema_version":"1.0","plans_revision":"5","plans":[$plans]}
        """.trimIndent())

    private fun quoteEvent(expiresAt: String = "2026-09-19T15:35:00Z"): JSONObject = JSONObject(
        """
        {"v":1,"attempt_id":"attempt","type":"quote_create_result","state":"ok",
         "request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-19T15:20:00Z",
         "schema_version":"1.0","quote_id":"q-1","amount":{"amount_minor":30000,"currency":"RUB"},
         "duration_code":"days:30","device_limit":2,"method":"card","expires_at":"$expiresAt"}
        """.trimIndent())

    private fun paymentEvent(
        type: String = "payment_create_result",
        status: String = "paid",
        applicationState: String = "applied",
        reference: String? = null,
    ): JSONObject {
        val referenceValue = reference?.let { "\"$it\"" } ?: "null"
        return JSONObject(
            """
            {"v":1,"attempt_id":"attempt","type":"$type","state":"ok",
             "request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-19T15:20:00Z",
             "schema_version":"1.0","payment_id":"pay-1","payment_status":"$status",
             "checkout_reference":$referenceValue,"credited_entitlement_revision":"9",
             "access_application_state":"$applicationState"}
            """.trimIndent())
    }

    @Test
    fun plansListParsesStrictlyAndRendersExactLabels() {
        val event = PaymentsContract.parse(plansEvent()) as PaymentsEvent.Plans
        assertEquals("5", event.plansRevision)
        val plan = event.plans.single()
        assertEquals("p30", plan.planId)
        assertEquals("30 дней", plan.title)
        assertEquals("days:30", plan.durationCode)
        assertEquals(2, plan.baseDeviceLimit)
        assertEquals(30_000L, plan.amountMinor)
        assertEquals("RUB", plan.currency)
        assertEquals(listOf("card", "sbp"), plan.methods)
        assertEquals("30 дней · 300,00 RUB · Карта, СБП", PaymentsText.planLine(plan))
        assertEquals("30 дней", PaymentsText.durationLabel("days:30"))
        assertEquals("3 месяца", PaymentsText.durationLabel("months:3"))
        assertEquals("6 месяцев", PaymentsText.durationLabel("months:6"))
        assertNull(PaymentsText.durationLabel("days:7"))
    }

    @Test
    fun threeServerPlansKeepTheirOwnDurationAndAmount() {
        val plans = listOf(
            Triple("days:30", "30 дней", 20_000),
            Triple("months:3", "3 месяца", 48_000),
            Triple("months:6", "6 месяцев", 84_000),
        ).mapIndexed { index, (duration, title, amount) ->
            JSONObject().put("plan_id", "plan-$index").put("title", title)
                .put("duration_code", duration).put("base_device_limit", 2)
                .put("amount", JSONObject().put("amount_minor", amount).put("currency", "RUB"))
                .put("methods", JSONArray().put("sbp"))
        }
        val event = PaymentsContract.parse(plansEvent(plans.joinToString(","))) as PaymentsEvent.Plans
        assertEquals(listOf("30 дней · 200,00 RUB · СБП", "3 месяца · 480,00 RUB · СБП",
            "6 месяцев · 840,00 RUB · СБП"), event.plans.map(PaymentsText::planLine))
    }

    @Test
    fun quoteParsesStrictlyAndRendersLocalExpiry() {
        val event = PaymentsContract.parse(quoteEvent()) as PaymentsEvent.Quote
        assertEquals("q-1", event.quote.quoteId)
        assertEquals(30_000L, event.quote.amountMinor)
        assertEquals("card", event.quote.method)
        assertEquals("2026-09-19T15:35:00Z", event.quote.expiresAt)
        assertEquals("30 дней · 300,00 RUB · Карта · действует до 19.09.2026 18:35 (местное время)",
            PaymentsText.quoteLine(event.quote, moscow))
        assertFalse(PurchaseFlow.quoteExpired(
            PurchaseState(quote = event.quote), java.time.Instant.parse("2026-09-19T15:34:00Z")))
        assertTrue(PurchaseFlow.quoteExpired(
            PurchaseState(quote = event.quote), java.time.Instant.parse("2026-09-19T15:35:00Z")))
    }

    @Test
    fun paymentParsesStrictlyAndMapsTruthfulStatuses() {
        val paid = (PaymentsContract.parse(paymentEvent()) as PaymentsEvent.Payment).payment
        assertEquals("pay-1", paid.paymentId)
        assertEquals("paid", paid.paymentStatus)
        assertNull(paid.checkoutReference)
        assertEquals("9", paid.creditedEntitlementRevision)
        assertEquals("applied", paid.accessApplicationState)
        assertEquals("Оплата получена. Подтверждаем подписку по серверу…", PaymentsText.paymentStatusText(paid))
        assertNull(PaymentsText.checkoutReferenceText(paid))

        val withReference = (PaymentsContract.parse(paymentEvent(
            status = "pending", applicationState = "not_requested", reference = "ref-77"))
            as PaymentsEvent.Payment).payment
        assertEquals("Ожидаем оплату.", PaymentsText.paymentStatusText(withReference))
        assertEquals("Код оплаты: ref-77", PaymentsText.checkoutReferenceText(withReference))
        assertNull(PaymentsText.applicationStateText("not_requested"))
        assertEquals("Заявка на доступ обрабатывается сервером.",
            PaymentsText.applicationStateText("pending"))

        assertEquals("Платёж не прошёл.", PaymentsText.paymentStatusText(
            (PaymentsContract.parse(paymentEvent(status = "failed", applicationState = "retryable_failure"))
                as PaymentsEvent.Payment).payment))
        assertEquals("Срок оплаты истёк.", PaymentsText.paymentStatusText(
            (PaymentsContract.parse(paymentEvent(status = "expired", applicationState = "not_requested"))
                as PaymentsEvent.Payment).payment))
        assertEquals("Платёж возвращён.", PaymentsText.paymentStatusText(
            (PaymentsContract.parse(paymentEvent(status = "refunded", applicationState = "not_requested"))
                as PaymentsEvent.Payment).payment))
        assertEquals("Платёж оспаривается.", PaymentsText.paymentStatusText(
            (PaymentsContract.parse(paymentEvent(status = "disputed", applicationState = "not_requested"))
                as PaymentsEvent.Payment).payment))
        // Unknown statuses cannot come through the strict parser, but the mapping stays
        // truthful rather than crashing or claiming success if one ever does.
        assertEquals("Статус оплаты неизвестен.", PaymentsText.paymentStatusText(
            PaymentStatusView("pay-1", "weird", null, null, "not_requested")))
    }

    @Test
    fun providerUnavailableAndPurchaseUnavailableAreClearStates() {
        val unavailable = PaymentsContract.parse(JSONObject(
            """
            {"v":1,"attempt_id":"attempt","type":"payment_create_result","state":"error",
             "code":"PROVIDER_UNAVAILABLE"}
            """.trimIndent())) as PaymentsEvent.Failure
        assertEquals("payment_create_result", unavailable.type)
        val state = PurchaseFlow.failure(null, unavailable.code)
        assertEquals(PurchaseFlow.UNAVAILABLE, state.phase)
        val registration = AccountAccessProjection.Registration("registered", true, false, null, false)
        assertEquals(PaymentsText.PROVIDER_UNAVAILABLE_TEXT,
            PaymentsText.purchaseStatus(state, registration, moscow))
        // purchase_available=false shows the same honest unavailable state, never a dead button claim.
        assertEquals(PaymentsText.UNAVAILABLE_TEXT,
            PaymentsText.purchaseStatus(PurchaseState(), registration, moscow))
        assertTrue(PaymentsText.UNAVAILABLE_TEXT.contains("Покупка временно недоступна"))
    }

    @Test
    fun strictParserRejectsUnknownMissingAndOutOfEnumFields() {
        fun rejected(event: JSONObject) {
            assertThrows(IllegalStateException::class.java) { PaymentsContract.parse(event) }
        }
        rejected(JSONObject(
            """
            {"v":1,"attempt_id":"attempt","type":"plans_list_result","state":"ok",
             "request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-19T15:20:00Z",
             "schema_version":"1.0","plans_revision":"5","plans":[],"extra":1}
            """.trimIndent()))
        rejected(JSONObject(plansEvent().toString()).apply { remove("plans_revision") })
        rejected(JSONObject(quoteEvent().toString()).put("state", "weird"))
        rejected(JSONObject(quoteEvent().toString()).put("schema_version", "2.0"))
        rejected(JSONObject(paymentEvent().toString()).put("payment_status", "weird"))
        rejected(JSONObject(paymentEvent().toString()).put("access_application_state", "weird"))
        rejected(JSONObject(paymentEvent().toString()).apply { remove("checkout_reference") })
        rejected(JSONObject(
            """
            {"v":1,"attempt_id":"attempt","type":"quote_create_result","state":"ok",
             "request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-19T15:20:00Z",
             "schema_version":"1.0","quote_id":"q-1","amount":{"amount_minor":30000,"currency":"RUB"},
             "duration_code":"days:7","device_limit":2,"method":"card","expires_at":"2026-09-19T15:35:00Z"}
            """.trimIndent()))
        rejected(JSONObject(
            """
            {"v":1,"attempt_id":"attempt","type":"quote_create_result","state":"ok",
             "request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-19T15:20:00Z",
             "schema_version":"1.0","quote_id":"q-1","amount":{"amount_minor":30000,"currency":"RUB"},
             "duration_code":"days:30","device_limit":2,"method":"cash","expires_at":"2026-09-19T15:35:00Z"}
            """.trimIndent()))
        rejected(JSONObject(quoteEvent().toString())
            .put("amount", JSONObject().put("amount_minor", 30000).put("currency", "rub")))
        rejected(JSONObject(quoteEvent().toString())
            .put("amount", JSONObject().put("amount_minor", -1).put("currency", "RUB")))
        rejected(JSONObject(quoteEvent().toString())
            .put("amount", JSONObject().put("amount_minor", "30000").put("currency", "RUB")))
        rejected(JSONObject(plansEvent().toString()).put("plans", JSONArray(
            """[{"plan_id":"p30","title":"t","duration_code":"days:30","base_device_limit":2,
                "amount":{"amount_minor":0,"currency":"RUB"},"methods":["card","card"]}]""")))
        rejected(JSONObject(
            """
            {"v":1,"attempt_id":"attempt","type":"payment_create_result","state":"error",
             "code":"WEIRD_CODE"}
            """.trimIndent()))
    }

    @Test
    fun priceUsesTheExactMinorAmountAndCurrency() {
        assertEquals("300,00 RUB", PaymentsText.priceLabel(30_000, "RUB"))
        assertEquals("1 234 567,89 RUB", PaymentsText.priceLabel(123_456_789, "RUB"))
        assertEquals("0,00 RUB", PaymentsText.priceLabel(0, "RUB"))
        assertEquals("0,05 RUB", PaymentsText.priceLabel(5, "RUB"))
    }

    @Test
    fun noQrAndNoHostedCheckoutInTheFrozenVocabulary() {
        // The contract carries no QR field and no checkout capability field, so the host
        // must never offer either. This pin documents the frozen decision.
        assertFalse(PaymentsText.hostedCheckoutOffered)
        assertTrue(PaymentsContract.ERROR_CODES.contains("PROVIDER_UNAVAILABLE"))
        assertTrue(PaymentsContract.METHODS == setOf("card", "sbp", "crypto"))
    }

    private companion object {
        private const val PLAN_JSON =
            """{"plan_id":"p30","title":"30 дней","duration_code":"days:30","base_device_limit":2,
                "amount":{"amount_minor":30000,"currency":"RUB"},"methods":["card","sbp"]}"""
    }
}

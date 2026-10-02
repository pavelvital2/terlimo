package xyz.terlimo.test

import java.time.Instant
import java.time.ZoneOffset
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class PurchaseRecoveryErrorTextTest {
    private class Disk : PurchaseAttemptStore {
        var bytes: String? = null
        var writes = 0
        override fun read() = bytes
        override fun write(encoded: String) { bytes = encoded; writes++ }
    }

    private fun event(type: String): JSONObject {
        val rows = JSONArray(javaClass.getResource("/payment-reviewed-bridge.json")!!.readText())
        return (0 until rows.length()).map { rows.getJSONObject(it) }
            .first { it.getString("scenario") == "selected_renewal" &&
                it.getJSONObject("event").getString("type") == type }.getJSONObject("event")
    }

    private fun status(state: PurchaseState) = PaymentsText.purchaseStatus(state, null, ZoneOffset.UTC)

    @Test fun firstPayAndExplicitRecoveryShowCauseWithoutResolvingDurableAttempt() {
        val disk = Disk()
        val attempts = PurchaseAttempts(disk)
        val quoteEvent = event(PaymentsContract.TYPE_QUOTE_CREATE_RESULT)
        val quote = (PaymentsContract.parse(quoteEvent) as PaymentsEvent.Quote).quote
        val plans = (PaymentsContract.parse(event(PaymentsContract.TYPE_PLANS_LIST_RESULT)) as PaymentsEvent.Plans).plans
        val selection = PurchaseSelection.fromPlan(plans.first { it.planId == quote.product!!.planId },
            quote.method, quote.product!!.renewExtraSlotIds)!!
        attempts.beginQuote(selection)
        attempts.bindQuote(quote.quoteId, quoteEvent.toString())
        val saved = attempts.markCreate("test-account", quote)
        val bytes = disk.bytes
        val writes = disk.writes
        val firstPay = PurchaseState(phase = PurchaseFlow.QUOTE_READY, recovery = "unknown_create",
            ownerAccountRef = "test-account", quote = quote,
            quoteBinding = PurchaseQuoteBinding(quote, selection))
        val restarted = PurchaseAttempts(disk) { error("must not rotate keys") }
        val operation = restarted.recoveryOperation("test-account") as PurchaseOperation.Payment
        assertTrue(operation.recovery)
        assertEquals(quote.quoteId, operation.quoteId)
        assertEquals(saved, restarted.retryCreate("test-account"))
        val recovery = restarted.recoveryState("test-account")!!
        for ((code, cause) in mapOf(
            "SERVICE_UNAVAILABLE" to "Сервис оплаты временно недоступен.",
            "TRANSPORT" to "Нет связи с сервисом оплаты.",
            "QUOTE_EXPIRED" to "Срок предложения истёк.",
        )) {
            for (base in listOf(firstPay, recovery)) {
                val failed = PurchaseFlow.failure(PurchaseFlow.sending(base), code)
                val text = status(failed)
                assertTrue(text.startsWith(cause))
                assertTrue(text.contains("ещё не подтверждён"))
                assertTrue(text.contains("«Восстановить оплату»"))
                assertTrue(text.contains("Новая покупка недоступна"))
                assertFalse(text.contains("восстановить не удалось"))
                assertFalse(text.contains("заказ не создан"))
                assertFalse(text.contains("заново"))
                assertEquals(base.quote, failed.quote)
                assertEquals(base.ownerAccountRef, failed.ownerAccountRef)
                assertEquals(base.payment, failed.payment)
                assertTrue(PurchaseFlow.blocksNewPurchase(failed))
                assertNull(failed.createAck)
                assertFalse(failed.freshMeConfirmed)
                assertEquals(saved, restarted.current())
                assertEquals(bytes, disk.bytes)
                assertEquals(writes, disk.writes)
            }
        }
        assertTrue(PurchaseFlow.quoteExpired(recovery, Instant.parse("2027-01-01T00:00:00Z")))
        assertFalse(restarted.restart())
        assertEquals(bytes, disk.bytes)
        assertEquals(writes, disk.writes)
    }

    @Test fun unresolvedErrorsAreBoundedAndSendingStillWins() {
        val pending = PurchaseState(recovery = "unknown_create", phase = PurchaseFlow.AWAITING_PAYMENT)
        assertTrue(status(pending).startsWith("Результат отправленного запроса"))
        for (code in listOf("IDEMPOTENCY_CONFLICT", "untrusted upstream detail", "QUOTE_EXPIRED")) {
            val failed = PurchaseFlow.failure(pending, code)
            assertFalse(status(failed).contains(code))
            assertFalse(status(failed).contains("заново"))
            assertEquals(PaymentsText.PURCHASE_SENDING_TEXT, status(PurchaseFlow.sending(failed)))
        }
    }

    @Test fun ordinaryExpiredQuoteKeepsItsExistingNextStep() {
        assertEquals(PaymentsText.errorText("QUOTE_EXPIRED"),
            status(PurchaseFlow.failure(PurchaseState(), "QUOTE_EXPIRED")))
    }
}

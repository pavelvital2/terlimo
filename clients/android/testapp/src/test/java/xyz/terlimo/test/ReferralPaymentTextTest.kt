package xyz.terlimo.test

import java.time.ZoneOffset
import org.junit.Assert.*
import org.junit.Test

class ReferralPaymentTextTest {
    private val pricing = PaymentPricing(21200, 10000, 11200, "RUB", "referral_first_main", "referral-20261003-v1")
    private fun quote(pricing: PaymentPricing? = this.pricing) = PaymentQuote(
        "quote-frozen", if (pricing == null) 2000 else 11200, "RUB", "days:30", 4,
        "card", "2026-10-03T18:00:00Z", pricing = pricing)
    private fun payment(state: String = "reconciling") = PaymentStatusView(
        "original-order", "expired", null, null, "not_requested",
        pricing = pricing, referralDiscountState = state)

    @Test fun frozenQuoteShowsWholePriceAndDiscountWithoutLosingExtras() {
        val text = PaymentsText.quoteLine(quote(), ZoneOffset.UTC)
        assertTrue(text.contains("Стоимость: 212,00 RUB"))
        assertTrue(text.contains("Скидка по коду: 100,00 RUB"))
        assertTrue(text.contains("К оплате: 112,00 RUB"))
        assertFalse(PaymentsText.quoteLine(quote(null), ZoneOffset.UTC).contains("Скидка по коду"))
    }

    @Test fun reconcilingKeepsOriginalOrderAndDoesNotPromiseRelease() {
        val original = payment()
        for (phase in listOf(PurchaseFlow.ERROR, PurchaseFlow.AWAITING_PAYMENT, PurchaseFlow.CONFIRMED)) {
            val state = PurchaseState(phase = phase, payment = original, recovery = "known_payment")
            assertEquals("Проверяем закрытие предыдущего счёта", PaymentsText.purchaseStatus(state, null, ZoneOffset.UTC))
            assertTrue(PurchaseFlow.blocksNewPurchase(state))
            assertFalse(PurchaseFlow.terminalPayment(original))
        }
        assertEquals("Проверяем закрытие предыдущего счёта", PaymentsText.paymentStatusText(original))
        assertNull(PaymentsText.quoteHint(PurchaseState(phase = PurchaseFlow.NO_ORDER), true, false, true))
    }

    @Test fun genericReservationErrorNeverClaimsNoOrderOrRelease() {
        val unknown = PurchaseState(phase = PurchaseFlow.ERROR, recovery = "unknown_create",
            error = "REFERRAL_DISCOUNT_RESERVED")
        val text = PaymentsText.purchaseStatus(unknown, null, ZoneOffset.UTC)
        assertTrue(text.contains("Исходный запрос сохранён"))
        assertTrue(text.contains("Новая покупка недоступна"))
        assertFalse(text.contains("Заказ по этому запросу не создан"))
        val proof = PurchaseState(phase = PurchaseFlow.NO_ORDER)
        assertTrue(PaymentsText.purchaseStatus(proof, null, ZoneOffset.UTC).contains("Можно запросить новое предложение"))
        assertFalse(PaymentsText.errorText("REFERRAL_PRICE_UNSUPPORTED").contains("0"))
    }
}

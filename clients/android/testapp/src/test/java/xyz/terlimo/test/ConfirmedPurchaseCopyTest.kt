package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class ConfirmedPurchaseCopyTest {
    @Test fun confirmedResultDoesNotInheritOldQuoteHint() {
        val state = PurchaseState(phase = PurchaseFlow.CONFIRMED)
        assertNull(PaymentsText.quoteHint(state, true, false, false))
        assertNull(PaymentsText.quoteHint(state.copy(sending = true), true, false, false))
        assertTrue(PaymentsText.purchaseStatus(state, null, java.time.ZoneOffset.UTC)
            .startsWith("Оплата подтверждена сервером. Доступ обновлён."))
        assertEquals(PaymentsText.PRICE_WAIT_TEXT,
            PaymentsText.quoteHint(state.copy(sending = true), true, false, true))
        assertEquals(PaymentsText.PRICE_RETRY_TEXT,
            PaymentsText.quoteHint(PurchaseState(phase = PurchaseFlow.ERROR), true, false, true))
        assertNull(PaymentsText.quoteHint(state.copy(recovery = "known_payment"), true, false, true))
        assertNull(PaymentsText.quoteHint(state.copy(phase = PurchaseFlow.EXPIRED_NO_ORDER), true, false, true))
    }
}

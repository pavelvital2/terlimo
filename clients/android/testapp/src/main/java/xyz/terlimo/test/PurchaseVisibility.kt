package xyz.terlimo.test

/**
 * Host visibility policy of the S5 purchase controls on the existing "Подписка" surface.
 *
 * It exists so the exact conditions are unit-testable independently of the Activity: the
 * quote-request control is offered only with no quote yet, the quote line and the pay control
 * only with an accepted quote, and a parked paid order awaiting Telegram binding (S5 §3.2C)
 * hides the quote control, the quote line and the pay control — so a second payment is never
 * offered while the existing order only needs registration.
 */
internal object PurchaseVisibility {
    /** The quote line is shown only for an accepted quote and not while a paid order is parked. */
    fun quoteLineVisible(quote: PaymentQuote?, paidAwaitingBinding: Boolean): Boolean =
        quote != null && !paidAwaitingBinding

    /** «Получить предложение» is offered only while there is no quote and nothing is parked. */
    fun quoteButtonVisible(quote: PaymentQuote?, paidAwaitingBinding: Boolean): Boolean =
        quote == null && !paidAwaitingBinding

    /** «Оплатить» is offered only for an accepted quote and not while a paid order is parked. */
    fun payVisible(quote: PaymentQuote?, paidAwaitingBinding: Boolean): Boolean =
        quote != null && !paidAwaitingBinding
}

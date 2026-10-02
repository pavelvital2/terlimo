package xyz.terlimo.test

/** Visibility of the confirmed price and Pay; quote requests are internal to selection. */
internal object PurchaseVisibility {
    /** The quote line is shown only for an accepted quote and not while a paid order is parked. */
    fun quoteLineVisible(quote: PaymentQuote?, paidAwaitingBinding: Boolean): Boolean =
        quote != null && !paidAwaitingBinding

    /** «Оплатить» is offered only for an accepted quote and not while a paid order is parked. */
    fun payVisible(quote: PaymentQuote?, paidAwaitingBinding: Boolean): Boolean =
        quote != null && !paidAwaitingBinding
}

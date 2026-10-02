package xyz.terlimo.test

/** Server quote snapshot. Slot IDs identify purchased places, never devices. */
internal data class PaymentProduct(
    val kind: String,
    val planId: String,
    val deviceDelta: Int,
    val targetEntitlementId: String?,
    val targetValidUntil: String?,
    val validFrom: String,
    val validUntil: String,
    val renewExtraSlotIds: List<String>,
    val baseAmountMinor: Long,
    val extraAmountMinor: Long,
    val deviceLimit: Int,
    val extraSlots: List<PaymentExtraSlot>,
)

internal data class PaymentExtraSlot(
    val slotId: String,
    val expiresAt: String,
    val renewAmountMinor: Long?,
)

/** Actual server credit, which may have a different period from the quote. */
internal data class CreditedPaymentProduct(
    val validFrom: String,
    val validUntil: String?,
    val deviceLimit: Int,
    val currentDeviceLimit: Int,
)

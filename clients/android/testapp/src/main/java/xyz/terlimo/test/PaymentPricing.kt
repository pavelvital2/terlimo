package xyz.terlimo.test

import org.json.JSONObject

/** Frozen server pricing; eligibility and discount are never calculated by the host. */
internal data class PaymentPricing(
    val baseAmountMinor: Long,
    val discountMinor: Long,
    val payableAmountMinor: Long,
    val currency: String,
    val discountKind: String,
    val termsVersion: String,
) {
    fun validFor(amountMinor: Long, currency: String, product: PaymentProduct?): Boolean {
        if (!valid() || amountMinor != payableAmountMinor || currency != this.currency || product == null) return false
        if (!(product.kind == "subscription" && product.deviceDelta == 0 &&
            product.baseAmountMinor > discountMinor && product.extraAmountMinor >= 0 &&
            // Subtraction proves the sum without allowing signed overflow in MAIN+extras.
            baseAmountMinor >= product.baseAmountMinor &&
            baseAmountMinor - product.baseAmountMinor == product.extraAmountMinor)) return false
        val selected = HashSet<String>()
        val slots = product.extraSlots.groupBy { it.slotId.lowercase() }
        var extraTotal = 0L
        for (id in product.renewExtraSlotIds) {
            val normalized = id.lowercase()
            if (!selected.add(normalized)) return false
            val slot = slots[normalized]?.singleOrNull() ?: return false
            val price = slot.renewAmountMinor ?: return false
            if (price <= 0 || extraTotal > Long.MAX_VALUE - price) return false
            extraTotal += price
        }
        return extraTotal == product.extraAmountMinor
    }

    fun valid(): Boolean = currency == "RUB" && discountKind == "referral_first_main" &&
        termsVersion == "referral-20261003-v1" && discountMinor == 10_000L &&
        baseAmountMinor > discountMinor && payableAmountMinor > 0 &&
        baseAmountMinor - discountMinor == payableAmountMinor
}

/** Present only on a decoded, correlated native create response with authoritative no-order proof. */
internal data class PaymentCreateResolution(
    val kind: String, val quoteId: String, val requestIdempotencyKey: String, val reason: String,
) {
    fun validFor(code: String): Boolean = kind == "no_order" && UUID.matches(quoteId) &&
        requestIdempotencyKey.length in 16..128 &&
        requestIdempotencyKey.none { it.code < 32 || it.code == 127 } &&
        ((code == "REFERRAL_DISCOUNT_RESERVED" && reason == "referral_discount_reserved") ||
         (code == "QUOTE_EXPIRED" && reason == "referral_quote_changed"))

    fun matches(quoteId: String, key: String): Boolean = this.quoteId == quoteId && requestIdempotencyKey == key

    private companion object {
        val UUID = Regex("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
    }
}

internal object PaymentPricingCodec {
    fun parse(value: JSONObject): PaymentPricing {
        check(value.keys().asSequence().toSet() == setOf("base_amount_minor", "discount_minor", "payable_amount_minor",
            "currency", "discount_kind", "terms_version")) { "PAYMENTS_INVALID" }
        return PaymentPricing(integer(value, "base_amount_minor"), integer(value, "discount_minor"),
            integer(value, "payable_amount_minor"), text(value, "currency"), text(value, "discount_kind"),
            text(value, "terms_version")).also { check(it.valid()) { "PAYMENTS_INVALID" } }
    }

    fun encode(value: PaymentPricing): JSONObject {
        check(value.valid()) { "PAYMENTS_INVALID" }
        return JSONObject().put("base_amount_minor", value.baseAmountMinor).put("discount_minor", value.discountMinor)
            .put("payable_amount_minor", value.payableAmountMinor).put("currency", value.currency)
            .put("discount_kind", value.discountKind).put("terms_version", value.termsVersion)
    }

    private fun integer(source: JSONObject, key: String): Long {
        val value = source.get(key)
        check(value is Int || value is Long) { "PAYMENTS_INVALID" }
        return (value as Number).toLong()
    }
    private fun text(source: JSONObject, key: String): String {
        val value = source.get(key)
        check(value is String) { "PAYMENTS_INVALID" }
        return value
    }
}

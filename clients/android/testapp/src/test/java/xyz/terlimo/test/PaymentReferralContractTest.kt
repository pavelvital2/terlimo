package xyz.terlimo.test

import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import java.io.File
import java.math.BigInteger

class PaymentReferralContractTest {
    private val quoteId = "00000000-0000-4000-8000-000000000001"
    private val slotId = "00000000-0000-4000-8000-000000000002"
    private val key = "payment-key-0001-original"
    private val from = "2026-10-03T00:00:00Z"
    private val until = "2026-11-03T00:00:00Z"

    private fun pricing(base: Long = 20700) = JSONObject().put("base_amount_minor", base).put("discount_minor", 10000)
        .put("payable_amount_minor", base - 10000).put("currency", "RUB")
        .put("discount_kind", "referral_first_main").put("terms_version", "referral-20261003-v1")
    private fun slot(id: String = slotId, amount: Any = 700) = JSONObject().put("slot_id", id)
        .put("expires_at", until).put("renew_amount_minor", amount)
    private fun product() = JSONObject().put("kind", "subscription").put("plan_id", "terlimo-30d")
        .put("device_delta", 0).put("target_entitlement_id", JSONObject.NULL).put("target_valid_until", JSONObject.NULL)
        .put("valid_from", from).put("valid_until", until).put("renew_extra_slot_ids", JSONArray().put(slotId))
        .put("base_amount_minor", 20000).put("extra_amount_minor", 700).put("device_limit", 3)
        .put("extra_slots", JSONArray().put(slot()))
    private fun envelope(type: String) = JSONObject().put("v", 1).put("attempt_id", "original-attempt")
        .put("type", type).put("state", "ok").put("request_id", "0123456789abcdef0123456789abcdef")
        .put("server_time", from).put("schema_version", "1.0")
    private fun quote() = envelope(PaymentsContract.TYPE_QUOTE_CREATE_RESULT).put("quote_id", quoteId)
        .put("duration_code", "days:30").put("device_limit", 3).put("method", "sbp")
        .put("expires_at", "2026-10-03T00:15:00Z").put("product", product()).put("pricing", pricing())
        .put("amount", JSONObject().put("amount_minor", 10700).put("currency", "RUB"))
    private fun payment() = envelope(PaymentsContract.TYPE_PAYMENT_CREATE_RESULT).put("payment_id", quoteId)
        .put("payment_status", "pending").put("checkout_reference", JSONObject.NULL)
        .put("credited_entitlement_revision", JSONObject.NULL).put("access_application_state", "not_requested")
        .put("product", product()).put("credit_state", "unapplied").put("credit_review_reason", JSONObject.NULL)
        .put("credited_product", JSONObject.NULL).put("pricing", pricing()).put("referral_discount_state", "reserved")
    private fun credit() = JSONObject().put("valid_from", from).put("valid_until", until).put("device_limit", 3)
        .put("current_device_limit", 3).put("pricing", pricing())
    private fun noOrder(reason: String = "referral_discount_reserved") = JSONObject().put("v", 1)
        .put("attempt_id", "original-attempt").put("type", PaymentsContract.TYPE_PAYMENT_CREATE_RESULT).put("state", "error")
        .put("code", if (reason == "referral_discount_reserved") "REFERRAL_DISCOUNT_RESERVED" else "QUOTE_EXPIRED")
        .put("reason", reason).put("http_status", 409).put("retryable", false)
        .put("request_id", "0123456789abcdef0123456789abcdef")
        .put("create_resolution", JSONObject().put("kind", "no_order").put("quote_id", quoteId)
            .put("request_idempotency_key", key).put("reason", reason))
    private fun rejected(value: JSONObject) = assertThrows(IllegalStateException::class.java) { PaymentsContract.parse(value) }

    @Test fun `discounted quote keeps MAIN base and full selected extra prices immutable`() {
        val parsed = (PaymentsContract.parse(quote()) as PaymentsEvent.Quote).quote
        assertEquals(10700L, parsed.amountMinor)
        assertEquals(20700L, parsed.pricing!!.baseAmountMinor)
        assertEquals(10000L, parsed.pricing!!.discountMinor)
        assertEquals(20000L, parsed.product!!.baseAmountMinor)
        assertEquals(700L, parsed.product!!.extraAmountMinor)
        assertEquals(listOf(slotId), parsed.product!!.renewExtraSlotIds)
        assertEquals(until, parsed.product!!.validUntil)
        assertEquals(parsed.pricing, PaymentPricingCodec.parse(PaymentPricingCodec.encode(parsed.pricing!!)))
    }

    @Test fun `create status and credit retain exact pricing without making reconciling terminal`() {
        val quote = (PaymentsContract.parse(quote()) as PaymentsEvent.Quote).quote
        for (state in PaymentsContract.REFERRAL_DISCOUNT_STATES) {
            val parsed = (PaymentsContract.parse(payment().put("type", PaymentsContract.TYPE_PAYMENT_GET_RESULT)
                .put("referral_discount_state", state)) as PaymentsEvent.Payment).payment
            assertEquals(quote.pricing, parsed.pricing)
            assertEquals(state, parsed.referralDiscountState)
            assertNull(parsed.creditedProduct)
        }
        val paid = (PaymentsContract.parse(payment().put("payment_status", "paid").put("credit_state", "applied")
            .put("referral_discount_state", "consumed").put("credited_product", credit())) as PaymentsEvent.Payment).payment
        assertEquals(quote.pricing, paid.pricing)
        assertEquals(paid.pricing, paid.creditedProduct!!.pricing)
    }

    @Test fun `pricing rejects null omission unknown fields and all numeric coercion and overflow`() {
        rejected(quote().put("pricing", JSONObject.NULL))
        for (field in listOf("base_amount_minor", "discount_minor", "payable_amount_minor")) {
            for (raw in listOf<Any>(JSONObject.NULL, "10000", 10000.5, BigInteger("9223372036854775808"))) {
                rejected(quote().apply { getJSONObject("pricing").put(field, raw) })
            }
            rejected(quote().apply { getJSONObject("pricing").remove(field) })
        }
        rejected(quote().apply { getJSONObject("pricing").put("extra", 1) })
        for ((field, value) in listOf("currency" to "USD", "discount_kind" to "automatic", "terms_version" to "future")) {
            rejected(quote().apply { getJSONObject("pricing").put(field, value) })
            rejected(quote().apply { getJSONObject("pricing").put(field, JSONObject.NULL) })
        }
    }

    @Test fun `pricing validates exact totals and rejects discount moved from MAIN into extras`() {
        rejected(quote().apply { getJSONObject("amount").put("amount_minor", 10600) })
        rejected(quote().apply { getJSONObject("pricing").put("payable_amount_minor", 10600) })
        rejected(quote().apply { getJSONObject("pricing").put("discount_minor", 9900) })
        rejected(quote().apply { getJSONObject("product").put("base_amount_minor", 10000) })
        rejected(quote().put("product", JSONObject.NULL))
        rejected(quote().apply { getJSONObject("product").put("extra_amount_minor", 600)
            getJSONObject("pricing").put("base_amount_minor", 20600).put("payable_amount_minor", 10600)
            getJSONObject("amount").put("amount_minor", 10600) })
        rejected(quote().apply { getJSONObject("product").put("extra_slots", JSONArray()) })
        rejected(quote().apply { getJSONObject("product").getJSONArray("extra_slots").getJSONObject(0)
            .put("renew_amount_minor", JSONObject.NULL) })
    }

    @Test fun `pricing permits large exact integer totals but rejects MAIN extras overflow`() {
        val large = quote().put("pricing", pricing(Long.MAX_VALUE)).apply {
            getJSONObject("product").put("base_amount_minor", Long.MAX_VALUE - 700)
            getJSONObject("amount").put("amount_minor", Long.MAX_VALUE - 10000)
        }
        assertEquals(Long.MAX_VALUE, (PaymentsContract.parse(large) as PaymentsEvent.Quote).quote.pricing!!.baseAmountMinor)
        rejected(quote().put("pricing", pricing(Long.MAX_VALUE)).apply {
            getJSONObject("product").put("base_amount_minor", Long.MAX_VALUE)
            getJSONObject("amount").put("amount_minor", Long.MAX_VALUE - 10000)
        })
    }

    @Test fun `credit requires the exact payment pricing and discount state cannot occur alone`() {
        rejected(payment().put("credited_product", credit().apply { remove("pricing") }))
        rejected(payment().put("credited_product", credit().put("pricing", pricing(20701))))
        rejected(payment().apply { remove("pricing") })
        rejected(payment().put("referral_discount_state", JSONObject.NULL))
        rejected(payment().put("referral_discount_state", "released"))
        rejected(payment().put("credited_product", credit().put("pricing", JSONObject.NULL)))
        rejected(payment().apply { remove("pricing"); remove("referral_discount_state") }.put("credited_product", credit()))
    }

    @Test fun `no pricing old TEST amounts nullable products addon and credit remain compatible`() {
        for (amount in listOf(1000, 2000)) {
            val old = quote().apply { remove("pricing"); put("product", JSONObject.NULL); put("device_limit", 2)
                getJSONObject("amount").put("amount_minor", amount) }
            val parsed = (PaymentsContract.parse(old) as PaymentsEvent.Quote).quote
            assertNull(parsed.pricing)
            assertEquals(amount.toLong(), parsed.amountMinor)
        }
        val addon = quote().apply { remove("pricing") }.put("duration_code", "until:$until").apply {
            getJSONObject("product").put("kind", "device_addon").put("plan_id", "terlimo-extra-device")
                .put("device_delta", 1).put("base_amount_minor", 0).put("target_entitlement_id", quoteId)
                .put("target_valid_until", until).put("renew_extra_slot_ids", JSONArray())
            getJSONObject("amount").put("amount_minor", 700)
        }
        assertNull((PaymentsContract.parse(addon) as PaymentsEvent.Quote).quote.pricing)
        val ordinary = payment().apply { remove("pricing"); remove("referral_discount_state") }
            .put("credited_product", credit().apply { remove("pricing") })
        assertNull((PaymentsContract.parse(ordinary) as PaymentsEvent.Payment).payment.creditedProduct!!.pricing)
    }

    @Test fun `only full create 409 proof produces bounded matching no order DTO`() {
        for (reason in listOf("referral_discount_reserved", "referral_quote_changed")) {
            val parsed = PaymentsContract.parse(noOrder(reason)) as PaymentsEvent.Failure
            assertTrue(parsed.referralNoOrder)
            assertFalse(parsed.expiredNoOrder)
            assertTrue(parsed.createResolution!!.matches(quoteId, key))
            assertFalse(parsed.createResolution!!.matches(quoteId, "payment-key-foreign"))
            assertFalse(parsed.createResolution!!.matches("00000000-0000-4000-8000-000000000009", key))
        }
    }

    @Test fun `malformed foreign incomplete or noncreate no order proof fails closed`() {
        val invalid = listOf(noOrder().put("http_status", "409"), noOrder().put("http_status", 409.5),
            noOrder().put("http_status", 404), noOrder().put("retryable", true), noOrder().put("retryable", "false"),
            noOrder().put("request_id", "bad"), noOrder().put("code", "QUOTE_EXPIRED"),
            noOrder().put("type", PaymentsContract.TYPE_PAYMENT_GET_RESULT), noOrder().put("create_resolution", JSONObject.NULL),
            noOrder().apply { remove("retryable") }, noOrder().apply { remove("request_id") }, noOrder().put("extra", 1),
            noOrder().apply { getJSONObject("create_resolution").remove("request_idempotency_key") },
            noOrder().apply { getJSONObject("create_resolution").put("request_idempotency_key", "short") },
            noOrder().apply { getJSONObject("create_resolution").put("quote_id", "legacy-Q") },
            noOrder().apply { getJSONObject("create_resolution").put("kind", "order") },
            noOrder().apply { getJSONObject("create_resolution").put("reason", "referral_quote_changed") },
            noOrder().apply { getJSONObject("create_resolution").put("account_ref", quoteId) })
        invalid.forEach(::rejected)
    }

    @Test fun `bare referral errors and unsupported price never imply no order or zero amount`() {
        val bare = JSONObject().put("v", 1).put("attempt_id", "original-attempt")
            .put("type", PaymentsContract.TYPE_PAYMENT_CREATE_RESULT).put("state", "error")
        for (code in listOf("REFERRAL_DISCOUNT_RESERVED", "REFERRAL_PRICE_UNSUPPORTED", "QUOTE_EXPIRED")) {
            val parsed = PaymentsContract.parse(JSONObject(bare.toString()).put("code", code)) as PaymentsEvent.Failure
            assertFalse(parsed.referralNoOrder)
            assertFalse(parsed.expiredNoOrder)
            assertNull(parsed.createResolution)
        }
        val changed = (PaymentsContract.parse(JSONObject(bare.toString()).put("code", "QUOTE_EXPIRED")
            .put("reason", "referral_quote_changed")) as PaymentsEvent.Failure)
        assertFalse(changed.referralNoOrder)
        assertFalse(changed.expiredNoOrder)
        val old = (PaymentsContract.parse(JSONObject(bare.toString()).put("code", "QUOTE_EXPIRED")
            .put("reason", PaymentsContract.EXPIRED_NO_ORDER_REASON)) as PaymentsEvent.Failure)
        assertTrue(old.expiredNoOrder)
        assertFalse(old.referralNoOrder)
    }

    @Test fun `shared representative native fixtures decode with same frozen server pricing`() {
        val root = generateSequence(File(System.getProperty("user.dir"))) { it.parentFile }
            .first { File(it, "docs/TERLIMO_IMPLEMENTATION/referral_20261003/payment-client-wire-fixtures.json").isFile }
        val fixtures = JSONObject(File(root, "docs/TERLIMO_IMPLEMENTATION/referral_20261003/payment-client-wire-fixtures.json").readText())
            .getJSONObject("cases")
        for (name in listOf("discounted_quote", "discounted_create", "discounted_reconciling", "discounted_credited",
            "no_order_reserved", "no_order_quote_changed", "legacy_10", "legacy_20", "legacy_addon")) {
            val scenario = fixtures.getJSONObject(name)
            val parsed = PaymentsContract.parse(scenario.getJSONObject("native"))
            when (parsed) {
                is PaymentsEvent.Quote -> if (name == "discounted_quote") assertEquals(
                    PaymentPricingCodec.parse(scenario.getJSONObject("server").getJSONObject("pricing")), parsed.quote.pricing)
                is PaymentsEvent.Payment -> if (name.startsWith("discounted_")) assertEquals(
                    PaymentPricingCodec.parse(scenario.getJSONObject("server").getJSONObject("pricing")), parsed.payment.pricing)
                is PaymentsEvent.Failure -> assertTrue(parsed.referralNoOrder)
                else -> fail("Unexpected payment fixture $name")
            }
        }
    }
}

package xyz.terlimo.test

import java.time.Instant
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class PurchaseProductFlowTest {
    private val now = Instant.parse("2026-10-02T12:00:00Z")
    private val end = "2026-12-01T00:00:00Z"
    private val firstSlot = PaymentExtraSlot("slot-a", "2026-11-01T00:00:00Z", 500)
    private val secondSlot = PaymentExtraSlot("slot-b", "2026-11-01T00:00:00Z", 700)

    private fun product(kind: String = "subscription", ids: List<String> = emptyList()) = PaymentProduct(
        kind = kind, planId = if (kind == "subscription") "terlimo-30d" else "terlimo-extra-device",
        deviceDelta = if (kind == "subscription") 0 else 1,
        targetEntitlementId = "target-a", targetValidUntil = "2026-11-01T00:00:00Z",
        validFrom = "2026-11-01T00:00:00Z", validUntil = end,
        renewExtraSlotIds = ids, baseAmountMinor = if (kind == "subscription") 20000 else 0,
        extraAmountMinor = 0, deviceLimit = 2 + ids.size,
        extraSlots = listOf(firstSlot, secondSlot),
    )

    private fun plan(product: PaymentProduct = product()) = PaymentPlan(
        product.planId, "Тариф", if (product.kind == "subscription") "days:30" else "until:${product.targetValidUntil}",
        2, if (product.kind == "subscription") 20000 else 500, "RUB", listOf("card", "sbp"), product)

    private fun loaded(plan: PaymentPlan = plan()) = PurchaseFlow.plansLoaded(null, "5", listOf(plan))

    private fun quote(product: PaymentProduct = product(), amount: Long = 20000) = PaymentQuote(
        "quote-a", amount, "RUB", if (product.kind == "subscription") "days:30" else "until:${product.targetValidUntil}",
        product.deviceLimit, "card", "2026-10-02T12:10:00Z", product)

    private fun paid(product: PaymentProduct = product(), creditState: String = "applied") = PaymentStatusView(
        "payment-a", "paid", null, "9", "not_requested", product, creditState,
        if (creditState == "needs_review") "target_expired" else null,
        if (creditState == "applied") CreditedPaymentProduct("2026-11-02T00:00:00Z", end, product.deviceLimit, 4) else null)

    private fun me(revision: String = "9", validUntil: String = end, limit: Int = 4) =
        AccountAccessParser.parse(JSONObject("""
            {"v":1,"attempt_id":"attempt","type":"account_access","access_version":1,
             "server_time":"2026-10-02T12:01:00Z","session_generation":"2",
             "previous_session_generation":"1","access_revision":"9",
             "account":{"state":"ACTIVE_PAID","telegram_linked":true,"binding_status":"active",
                "management_only":false,"account_ref":"acc-1"},
             "entitlement":{"type":"paid","status":"active","valid_from":"2026-09-01T00:00:00Z",
                "valid_until":"$validUntil","effective_device_limit":$limit,"slots_used":1,
                "revision":"$revision","perpetual_commercial":false},
             "onboarding":{"state":"not_started","started_by":"server_confirmed_first_connection",
                "started_at":null,"not_after":null,"duration_seconds":3600,"one_time":true,
                "extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
                "requires_hardware_id":false,"unit":"installation_fingerprint",
                "post_telegram_identity":"account_history_correlation",
                "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
             "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
                "data_access":"subscription_data","effective_deadline":"$validUntil"}}
        """.trimIndent()))

    @Test fun changedServerAmountRequiresTheExplicitOfferBinding() {
        val base = loaded()
        val changed = quote(amount = 20001)
        assertNull(PurchaseFlow.payableQuote(base.copy(phase = PurchaseFlow.QUOTE_READY, quote = changed), plan(), "card", now))
        val displayed = PurchaseFlow.quoteReady(base, changed)
        assertTrue(displayed.quotePriceChanged)
        assertEquals(changed, PurchaseFlow.payableQuote(displayed, plan(), "card", now))
        assertNull(PurchaseFlow.payableQuote(displayed.copy(quote = changed.copy(quoteId = "quote-other")), plan(), "card", now))
        assertNull(PurchaseFlow.payableQuote(PurchaseFlow.sending(displayed), plan(), "card", now))
        assertNull(PurchaseFlow.payableQuote(displayed, plan(), "card", now.plusSeconds(600)))
    }

    @Test fun slotSelectionAndTargetSnapshotMustMatchExactly() {
        val selected = PurchaseFlow.selectRenewExtraSlots(loaded(), listOf("slot-b", "slot-a"))
        assertEquals(listOf("slot-a", "slot-b"), selected.selectedRenewExtraSlotIds)
        val offered = quote(product(ids = listOf("slot-a", "slot-b")), 21200)
        val ready = PurchaseFlow.quoteReady(selected, offered)
        assertFalse(ready.quotePriceChanged)
        assertNotNull(PurchaseFlow.payableQuote(ready, plan(), "card", now))
        val changed = PurchaseFlow.selectRenewExtraSlots(ready, listOf("slot-a"))
        assertNull(changed.quote)
        assertNull(PurchaseFlow.payableQuote(changed.copy(quote = offered), plan(), "card", now))
        assertNull(PurchaseFlow.quoteReady(selected, offered.copy(product = offered.product!!.copy(
            targetEntitlementId = "target-other"))).quoteBinding)
        assertNull(PurchaseFlow.quoteReady(selected, offered, PurchaseSelection.fromPlan(plan(), "card")).quoteBinding)
        assertNull(PurchaseFlow.quoteReady(selected, offered.copy(product = offered.product!!.copy(
            renewExtraSlotIds = listOf("slot-a")))).quoteBinding)
    }

    @Test fun unknownRenewalPricesAndOverflowCannotBecomeOffers() {
        val unknown = plan(product().copy(extraSlots = listOf(firstSlot.copy(renewAmountMinor = null))))
        assertNull(PurchaseSelection.fromPlan(unknown, "card", listOf("slot-a")))
        assertNull(PurchaseSelection.fromPlan(plan(), "card", listOf("slot-a", "slot-a")))
        val huge = plan(product().copy(baseAmountMinor = Long.MAX_VALUE))
        val selected = PurchaseFlow.selectRenewExtraSlots(loaded(huge), listOf("slot-a"))
        assertNull(PurchaseFlow.quoteReady(selected, quote(product(ids = listOf("slot-a")), 1)).quoteBinding)
    }

    @Test fun addonUsesFiniteServerTargetAndCannotRenewOtherSlots() {
        val addon = plan(product("device_addon").copy(deviceLimit = 3))
        assertEquals(listOf(addon), PurchaseFlow.selectablePlans(listOf(addon)))
        assertNull(PurchaseSelection.fromPlan(addon, "card", listOf("slot-a")))
        val ready = PurchaseFlow.quoteReady(loaded(addon), quote(addon.product!!, 501))
        assertTrue(ready.quotePriceChanged)
        assertNotNull(PurchaseFlow.payableQuote(ready, addon, "card", now))
    }

    @Test fun needsReviewNeverConfirmsOrReleasesThePaidBarrierOnRefreshFailure() {
        val receipt = paid(creditState = "needs_review")
        val state = PurchaseFlow.paymentResult(loaded(), receipt)
        val refreshed = PurchaseFlow.plansLoaded(PurchaseFlow.onFreshMe(state, me()), "6", listOf(plan()))
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION, refreshed.phase)
        val failed = PurchaseFlow.plansFailure(refreshed, "TRANSPORT")
        assertEquals(receipt, failed.payment)
        assertTrue(PurchaseFlow.paidAwaitingBinding(failed))
        assertEquals(failed, PurchaseFlow.selectMethod(failed, "sbp"))
        assertNull(PurchaseFlow.payableQuote(PurchaseFlow.quoteReady(failed, quote()), plan(), "card", now))
    }

    @Test fun actualCreditAndFreshRevisionRequirePostPaidPlansInEitherOrder() {
        val paid = PurchaseFlow.paymentResult(loaded(), paid())
        val fresh = PurchaseFlow.onFreshMe(paid, me())
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION, fresh.phase)
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION,
            PurchaseFlow.plansLoaded(fresh, "6", emptyList()).phase)
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION,
            PurchaseFlow.plansLoaded(fresh, "6", listOf(plan().copy(product = null))).phase)
        assertEquals(PurchaseFlow.CONFIRMED, PurchaseFlow.plansLoaded(fresh, "6", listOf(plan())).phase)
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION, PurchaseFlow.plansLoaded(
            PurchaseFlow.onFreshMe(fresh, me("8")), "6", listOf(plan())).phase)
        val plansFirst = PurchaseFlow.plansLoaded(paid, "6", listOf(plan()))
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION, PurchaseFlow.onFreshMe(plansFirst, me("8")).phase)
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION, PurchaseFlow.onFreshMe(plansFirst, me(limit = 3)).phase)
        // The proposed quote start and actual start may differ; actual receipt governs.
        assertEquals(PurchaseFlow.CONFIRMED, PurchaseFlow.onFreshMe(plansFirst, me()).phase)
        // A quote is an immutable offer; concurrent server credits can change the actual
        // credited capacity. Compare fresh /me to the actual receipt, not the old offer.
        val actualChanged = plansFirst.copy(payment = plansFirst.payment!!.copy(
            creditedProduct = plansFirst.payment!!.creditedProduct!!.copy(deviceLimit = 3)))
        assertEquals(PurchaseFlow.CONFIRMED, PurchaseFlow.onFreshMe(actualChanged, me()).phase)
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION, PurchaseFlow.onFreshMe(
            plansFirst.copy(payment = plansFirst.payment!!.copy(creditedProduct = null)), me()).phase)
    }

    @Test fun freshPlansInvalidateTheOfferWhenTheServerTargetChanges() {
        val ready = PurchaseFlow.quoteReady(loaded(), quote())
        val refreshed = PurchaseFlow.plansLoaded(ready, "6", listOf(plan(product().copy(targetValidUntil = end))))
        assertNull(refreshed.quote)
        assertNull(refreshed.quoteBinding)
    }

    @Test fun aConfirmedReceiptIsReplacedOnlyByTheNextDisplayedOffer() {
        val confirmed = PurchaseFlow.plansLoaded(PurchaseFlow.onFreshMe(
            PurchaseFlow.paymentResult(loaded(), paid()), me()), "6", listOf(plan()))
        val selected = PurchaseFlow.selectRenewExtraSlots(confirmed, listOf("slot-a"))
        assertEquals(confirmed.payment, selected.payment)
        val next = PurchaseFlow.quoteReady(selected, quote(product(ids = listOf("slot-a")), 20500))
        assertEquals(PurchaseFlow.QUOTE_READY, next.phase)
        assertNull(next.payment)
        assertNotNull(next.quoteBinding)
    }
}

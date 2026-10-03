package xyz.terlimo.test

import java.time.Instant

/** Exact business selection; prices and quoted periods remain server-owned offers. */
internal data class PurchaseSelection(
    val planId: String,
    val durationCode: String,
    val method: String,
    val productKind: String? = null,
    val targetEntitlementId: String? = null,
    val targetValidUntil: String? = null,
    val renewExtraSlotIds: List<String> = emptyList(),
) {
    companion object {
        fun fromPlan(
            plan: PaymentPlan, method: String, renewExtraSlotIds: List<String> = emptyList(),
        ): PurchaseSelection? {
            if (method !in plan.methods || renewExtraSlotIds.map { it.lowercase() }.distinct().size != renewExtraSlotIds.size) return null
            val product = plan.product
            if (product?.kind != "subscription" && renewExtraSlotIds.isNotEmpty()) return null
            if (renewExtraSlotIds.any { id -> product?.extraSlots?.none {
                    it.slotId == id && it.renewAmountMinor != null
                } != false }) return null
            return PurchaseSelection(plan.planId, plan.durationCode, method, product?.kind,
                product?.targetEntitlementId, product?.targetValidUntil, renewExtraSlotIds.sorted())
        }
    }
}

/** The offer displayed by quoteReady, frozen together with its complete selection. */
internal data class PurchaseQuoteBinding(val quote: PaymentQuote, val selection: PurchaseSelection)

/**
 * Bounded host-local purchase flow state for the S5 payment surface. It is a pure
 * projection of accepted server results: [PurchaseFlow.CONFIRMED] is reachable only
 * through a fresh accepted /me projection whose entitlement is active — never from a
 * redirect, a checkout return or a payment status alone. A paid payment stays
 * [PurchaseFlow.AWAITING_CONFIRMATION] while the server has not confirmed the right.
 */
internal data class PurchaseState(
    val phase: String = PurchaseFlow.IDLE,
    val ownerAccountRef: String? = null,
    val recovery: String? = null,
    val plansRevision: String? = null,
    val plans: List<PaymentPlan> = emptyList(),
    val selectedPlanId: String? = null,
    val selectedMethod: String? = null,
    val selectedRenewExtraSlotIds: List<String> = emptyList(),
    val quoteBinding: PurchaseQuoteBinding? = null,
    val quotePriceChanged: Boolean = false,
    val freshMeConfirmed: Boolean = false,
    val confirmationPlansLoaded: Boolean = false,
    val quote: PaymentQuote? = null,
    val payment: PaymentStatusView? = null,
    /**
     * Correlated acknowledgement of the explicitly sent payment_create of the current
     * attempt; null until that create result arrives and dropped on every attempt
     * replacement/failure (see [PaymentCreateAck]).
     */
    val createAck: PaymentCreateAck? = null,
    /**
     * True while one purchase request is outstanding on the native stream (single-flight);
     * the UI shows waiting and disables pay/plan/method until the matching result arrives.
     */
    val sending: Boolean = false,
    val error: String? = null,
)

internal object PurchaseFlow {
    const val IDLE = "idle"
    const val PLANS = "plans"
    const val QUOTE_READY = "quote_ready"
    const val AWAITING_PAYMENT = "awaiting_payment"
    const val AWAITING_CONFIRMATION = "awaiting_confirmation"
    const val CONFIRMED = "confirmed"
    const val EXPIRED_NO_ORDER = "expired_no_order"
    const val UNAVAILABLE = "unavailable"
    const val ERROR = "error"

    /** Accepted account identity, independent of this installation's registration-link history. */
    fun usableAccount(me: AccountAccessProjection?): Boolean = me?.account?.let {
        !it.accountRef.isNullOrBlank() && it.telegramLinked && it.bindingStatus == "active"
    } == true

    /** Display/entry eligibility from accepted /me; serial prepare additionally requires freshness. */
    fun offered(me: AccountAccessProjection?): Boolean =
        usableAccount(me) && me?.registration?.purchaseAvailable == true

    /** Checked on the serial service prepare path, before keys/intent or native write.
     * Existing-order recovery keeps its separate strict durable account fence.
     */
    fun canPrepare(operation: PurchaseOperation, snapshot: AccountAccessSnapshot?, verifiedAccountRef: String?): Boolean {
        if (operation == PurchaseOperation.Recover || operation is PurchaseOperation.PaymentGet ||
            (operation is PurchaseOperation.Payment && operation.recovery)) return true
        val me = snapshot?.takeIf { it.current }?.projection ?: return false
        val account = me.account
        return offered(me) && account.accountRef == verifiedAccountRef
    }

    /** Plans need a known server duration, including a finite addon target. */
    fun selectablePlans(plans: List<PaymentPlan>): List<PaymentPlan> = plans.filter { plan ->
        (PaymentsText.durationLabel(plan.durationCode) != null ||
            (plan.product?.kind == "device_addon" && plan.durationCode.startsWith("until:") &&
                LocalStamp.parseUtc(plan.durationCode.removePrefix("until:")) != null)) &&
            plan.methods.any { PaymentsText.methodLabel(it) != null }
    }

    fun selection(current: PurchaseState?): PurchaseSelection? {
        val base = current ?: return null
        val plan = base.plans.firstOrNull { it.planId == base.selectedPlanId } ?: return null
        return PurchaseSelection.fromPlan(plan, base.selectedMethod ?: return null, base.selectedRenewExtraSlotIds)
    }

    fun plansLoaded(current: PurchaseState?, revision: String, plans: List<PaymentPlan>): PurchaseState {
        val base = current ?: PurchaseState()
        // Refresh catalog data without releasing the paid receipt or the no-Pay barrier.
        if (base.payment?.paymentStatus == "paid") return confirmIfReady(base.copy(
            plansRevision = revision, plans = plans,
            confirmationPlansLoaded = base.payment.product == null || plans.any { it.product != null },
            sending = false, error = null))
        if (blocksNewPurchase(base)) return base.copy(
            plansRevision = revision, plans = plans, sending = false, error = null)
        val selectable = selectablePlans(plans)
        val selected = selectable.firstOrNull { it.planId == base.selectedPlanId } ?: selectable.firstOrNull()
        val method = when {
            selected == null -> null
            base.selectedPlanId == selected.planId && base.selectedMethod in selected.methods -> base.selectedMethod
            else -> selected.methods.firstOrNull()
        }
        val ids = if (selected != null && selected.planId == base.selectedPlanId && selected.product?.kind == "subscription")
            base.selectedRenewExtraSlotIds.filter { id -> selected.product.extraSlots.any {
                it.slotId == id && it.renewAmountMinor != null
            } } else emptyList()
        val updated = base.copy(plansRevision = revision, plans = plans, selectedPlanId = selected?.planId,
            selectedMethod = method, selectedRenewExtraSlotIds = ids, sending = false, error = null)
        val sameSelection = selection(base) != null && selection(base) == selection(updated)
        return updated.copy(phase = if (sameSelection && base.quote != null) base.phase else PLANS,
            quote = if (sameSelection) base.quote else null,
            quoteBinding = if (sameSelection) base.quoteBinding else null,
            quotePriceChanged = sameSelection && base.quotePriceChanged,
            createAck = if (sameSelection) base.createAck else null)
    }

    fun selectPlan(current: PurchaseState?, planId: String): PurchaseState {
        val base = current ?: return PurchaseState()
        if (base.sending || blocksNewPurchase(base)) return base
        val plan = selectablePlans(base.plans).firstOrNull { it.planId == planId } ?: return base
        if (base.selectedPlanId == planId) return base
        return invalidateOffer(base.copy(selectedPlanId = plan.planId,
            selectedMethod = plan.methods.firstOrNull(), selectedRenewExtraSlotIds = emptyList()))
    }

    fun selectMethod(current: PurchaseState?, method: String): PurchaseState {
        val base = current ?: return PurchaseState()
        if (base.sending || blocksNewPurchase(base)) return base
        val plan = base.plans.firstOrNull { it.planId == base.selectedPlanId } ?: return base
        if (method !in plan.methods || base.selectedMethod == method) return base
        return invalidateOffer(base.copy(selectedMethod = method))
    }

    fun selectRenewExtraSlots(current: PurchaseState?, ids: List<String>): PurchaseState {
        val base = current ?: return PurchaseState()
        if (base.sending || blocksNewPurchase(base)) return base
        val plan = base.plans.firstOrNull { it.planId == base.selectedPlanId } ?: return base
        val selected = PurchaseSelection.fromPlan(plan, base.selectedMethod ?: return base, ids) ?: return base
        if (selected.renewExtraSlotIds == base.selectedRenewExtraSlotIds) return base
        return invalidateOffer(base.copy(selectedRenewExtraSlotIds = selected.renewExtraSlotIds))
    }

    private fun invalidateOffer(base: PurchaseState): PurchaseState = base.copy(
        phase = if (base.phase == CONFIRMED) CONFIRMED else PLANS,
        quote = null, quoteBinding = null, quotePriceChanged = false, createAck = null)

    /** Explicitly display a new exact server offer before its Pay can become actionable. */
    fun quoteReady(
        current: PurchaseState?, quote: PaymentQuote, selection: PurchaseSelection? = selection(current),
    ): PurchaseState {
        val base = current ?: PurchaseState()
        if (blocksNewPurchase(base)) return base.copy(sending = false)
        val plan = base.plans.firstOrNull { it.planId == base.selectedPlanId }
        if (selection == null || plan == null) return base.copy(phase = QUOTE_READY, quote = quote,
            quoteBinding = null, quotePriceChanged = false, error = null, createAck = null, sending = false)
        val expectedAmount = initialOfferAmount(plan, selection)
        if (selection != PurchaseFlow.selection(base) || !quoteMatchesSelection(quote, plan, selection) ||
            expectedAmount == null || (quote.product == null && quote.amountMinor != expectedAmount))
            return failure(invalidateOffer(base), "PAYMENT_STATE_INVALID")
        return base.copy(phase = QUOTE_READY, quote = quote, quoteBinding = PurchaseQuoteBinding(quote, selection),
            quotePriceChanged = quote.amountMinor != expectedAmount, error = null, createAck = null, sending = false,
            payment = if (base.phase == CONFIRMED) null else base.payment,
            freshMeConfirmed = false, confirmationPlansLoaded = false)
    }

    /** Sum only server renewal prices; no client prorata or inferred slot composition. */
    private fun initialOfferAmount(plan: PaymentPlan, selection: PurchaseSelection): Long? {
        return try {
            if (plan.product?.kind != "subscription") plan.amountMinor else {
                var amount = plan.product.baseAmountMinor
                for (id in selection.renewExtraSlotIds) {
                    val renewal = plan.product.extraSlots.firstOrNull { it.slotId == id }?.renewAmountMinor
                    if (renewal == null) return null
                    amount = Math.addExact(amount, renewal)
                }
                amount
            }
        } catch (_: ArithmeticException) { null }
    }

    private fun quoteMatchesSelection(quote: PaymentQuote, plan: PaymentPlan, selected: PurchaseSelection): Boolean {
        if (quote.method != selected.method || quote.durationCode != selected.durationCode ||
            quote.currency != plan.currency || quote.amountMinor <= 0) return false
        val product = quote.product
        if (product == null) return selected.productKind == null && selected.renewExtraSlotIds.isEmpty()
        return product.kind == selected.productKind && product.planId == selected.planId &&
            product.targetEntitlementId == selected.targetEntitlementId &&
            product.targetValidUntil == selected.targetValidUntil &&
            product.renewExtraSlotIds.distinct().size == product.renewExtraSlotIds.size &&
            product.renewExtraSlotIds.sorted() == selected.renewExtraSlotIds &&
            product.deviceDelta == plan.product?.deviceDelta && quote.deviceLimit == product.deviceLimit &&
            (product.kind != "subscription" || product.deviceLimit == plan.baseDeviceLimit + selected.renewExtraSlotIds.size)
    }

    /** Called only after the definitive no-create proof has been committed durably. */
    fun expiredNoOrderState(current: PurchaseState?, owner: String): PurchaseState = PurchaseState(
        phase = EXPIRED_NO_ORDER, ownerAccountRef = owner,
        plansRevision = current?.plansRevision, plans = current?.plans.orEmpty(),
    )

    fun quoteExpired(current: PurchaseState?, now: Instant): Boolean {
        val quote = current?.quote ?: return false
        val expiry = LocalStamp.parseUtc(quote.expiresAt) ?: return true
        return !now.isBefore(expiry)
    }

    /** Exact live quote for the selected server offer; shared by UI and service Pay guards. */
    fun payableQuote(
        current: PurchaseState?, plan: PaymentPlan?, method: String?, now: Instant,
    ): PaymentQuote? {
        if (current == null || plan == null || method == null || current.sending ||
            current.phase !in setOf(QUOTE_READY, AWAITING_PAYMENT) || blocksNewPurchase(current)) return null
        val quote = current.quote ?: return null
        val selected = selection(current) ?: return null
        val binding = current.quoteBinding ?: return null
        if (current.selectedPlanId != plan.planId || current.selectedMethod != method ||
            current.plans.firstOrNull { it.planId == plan.planId } != plan || method !in plan.methods ||
            binding.quote != quote || binding.selection != selected ||
            !quoteMatchesSelection(quote, plan, selected) ||
            (quote.product == null && quote.amountMinor != plan.amountMinor) || quoteExpired(current, now)) return null
        return quote
    }

    /**
     * A quote that expired is a deliberate new attempt: the caller must drop the stale
     * quote and request a new one (with new host-owned keys), never silently reuse it.
     */
    fun quoteExpiredState(current: PurchaseState?): PurchaseState =
        (current ?: PurchaseState()).copy(
            phase = ERROR, quote = null, quoteBinding = null, quotePriceChanged = false,
            error = "QUOTE_EXPIRED", createAck = null, sending = false)

    /** Marks the explicit purchase request as outstanding (single-flight captured). */
    fun sending(current: PurchaseState?): PurchaseState =
        (current ?: PurchaseState()).copy(sending = true)

    fun paymentResult(current: PurchaseState?, payment: PaymentStatusView): PurchaseState {
        val phase = when (payment.paymentStatus) {
            "paid" -> AWAITING_CONFIRMATION
            "created", "pending" -> AWAITING_PAYMENT
            else -> ERROR
        }
        val error = when (payment.paymentStatus) {
            "failed" -> "PAYMENT_FAILED"
            "expired" -> "PAYMENT_EXPIRED"
            "refunded" -> "PAYMENT_REFUNDED"
            "disputed" -> "PAYMENT_DISPUTED"
            "created", "pending", "paid" -> null
            else -> "PAYMENT_STATUS_UNKNOWN"
        }
        val base = current ?: PurchaseState()
        val sameReceipt = base.payment == payment
        return base.copy(phase = if (sameReceipt && base.phase == CONFIRMED) CONFIRMED else phase,
            payment = payment, error = error, sending = false,
            freshMeConfirmed = sameReceipt && base.freshMeConfirmed,
            confirmationPlansLoaded = sameReceipt && base.confirmationPlansLoaded)
    }

    /**
     * The payment-get status is only ever refreshed from server data, never locally advanced,
     * and it never sets or refreshes the create acknowledgement: a get result can neither
     * satisfy nor replace the explicit pay correlation.
     */
    fun paymentGetResult(current: PurchaseState?, payment: PaymentStatusView): PurchaseState =
        paymentResult(current, payment)

    /**
     * A server-confirmed paid payment whose entitlement is not applied yet: the order is paid
     * but binding has not happened, so a fresh /me cannot confirm it. The client must not offer
     * another payment and must offer the mandatory Telegram registration instead (S5 §3.2C).
     */
    fun blocksNewPurchase(state: PurchaseState?): Boolean = state?.recovery != null || paidAwaitingBinding(state)

    fun paidAwaitingBinding(state: PurchaseState?): Boolean {
        val base = state ?: return false
        return base.payment?.paymentStatus == "paid" && base.phase != CONFIRMED
    }

    /** A correlated payment_create result: the payment state plus the ack of the sent create. */
    fun paymentCreateResult(
        current: PurchaseState?, payment: PaymentStatusView, ack: PaymentCreateAck,
    ): PurchaseState = paymentResult(current, payment).copy(createAck = ack)

    fun failure(current: PurchaseState?, code: String): PurchaseState {
        val base = current ?: PurchaseState()
        if (base.payment?.paymentStatus == "paid") return base.copy(error = code, sending = false)
        return if (PaymentsText.isUnavailable(code))
            base.copy(phase = UNAVAILABLE, error = code, createAck = null, sending = false)
        else base.copy(phase = ERROR, error = code, createAck = null, sending = false)
    }

    /** A failed refresh invalidates offers while preserving any paid receipt and barrier. */
    fun plansFailure(current: PurchaseState?, code: String): PurchaseState {
        val base = current ?: PurchaseState()
        if (blocksNewPurchase(base)) return base.copy(
            error = code, sending = false, confirmationPlansLoaded = false)
        return failure(base.copy(plansRevision = null, plans = emptyList(), selectedPlanId = null,
            selectedMethod = null, selectedRenewExtraSlotIds = emptyList(), quote = null,
            quoteBinding = null, quotePriceChanged = false, createAck = null, sending = false), code)
    }

    /** A definitive terminal payment state ends the attempt; retry becomes a new key pair. */
    fun terminalPayment(payment: PaymentStatusView): Boolean =
        payment.paymentStatus in setOf("failed", "expired", "refunded", "disputed")

    /**
     * Confirmation policy: a fresh accepted /me must show an active paid entitlement
     * at the revision credited by this payment. An older active trial or paid right
     * cannot confirm a new purchase. The entitlement is only projected here.
     */
    fun onFreshMe(current: PurchaseState?, projection: AccountAccessProjection): PurchaseState {
        val base = current ?: return PurchaseState()
        if (base.phase != AWAITING_CONFIRMATION) return base
        val unconfirmed = base.copy(freshMeConfirmed = false)
        val payment = base.payment ?: return unconfirmed
        if (payment.creditState == "needs_review" || payment.creditState == "unapplied") return unconfirmed
        val creditedRevision = payment.creditedEntitlementRevision ?: return unconfirmed
        val entitlement = projection.entitlement
        if (entitlement.status != "active" || entitlement.type != "paid" ||
            entitlement.revision != creditedRevision) return unconfirmed
        if (payment.product != null) {
            val actual = payment.creditedProduct ?: return unconfirmed
            if (payment.creditState != "applied" || entitlement.validUntil != actual.validUntil ||
                entitlement.effectiveDeviceLimit != actual.currentDeviceLimit ||
                LocalStamp.parseUtc(actual.validFrom) == null ||
                (actual.validUntil != null && LocalStamp.parseUtc(actual.validUntil) == null)) return unconfirmed
        }
        return confirmIfReady(base.copy(freshMeConfirmed = true))
    }

    private fun confirmIfReady(base: PurchaseState): PurchaseState {
        val payment = base.payment ?: return base
        if (base.phase != AWAITING_CONFIRMATION || payment.paymentStatus != "paid" ||
            payment.creditState in setOf("needs_review", "unapplied") || !base.freshMeConfirmed) return base
        if (payment.creditState != null && !base.confirmationPlansLoaded) return base
        if (payment.product != null && (!base.confirmationPlansLoaded || payment.creditedProduct == null ||
                payment.creditState != "applied")) return base
        return base.copy(phase = CONFIRMED, error = null)
    }

    fun restart(): PurchaseState = PurchaseState()
}

/** The frozen host->native payment actions; the host sends exactly these fields. */
internal sealed class PurchaseOperation {
    object Plans : PurchaseOperation()
    object Recover : PurchaseOperation()
    data class Quote(
        val planId: String, val durationCode: String, val method: String,
        val renewExtraSlotIds: List<String> = emptyList(),
        val selection: PurchaseSelection? = null,
    ) : PurchaseOperation()
    data class Payment(val quoteId: String, val recovery: Boolean = false) : PurchaseOperation()
    data class PaymentGet(val paymentId: String) : PurchaseOperation()
}

internal enum class PurchaseTapAction { SEND_NOW, START_SERVICE, ERROR, IGNORE }

/**
 * Explicit purchase action gate, the S5 sibling of [TrialActivateGate]. Payment actions
 * need a live managed child, so with no active attempt the tap starts one bounded
 * service-only attempt through the existing linkless `begin("")` path and the requested
 * operation is sent exactly once, after the first accepted /me on that attempt. The gate
 * never derives eligibility, never generates keys and never bypasses the server. It
 * never starts anything from a background/resume path: only an explicit UI tap.
 */
internal class PurchaseGate {
    private var startPending = false
    private var coldAttempt: String? = null
    private var pendingOperation: PurchaseOperation? = null

    /** One explicit UI tap. `attemptId` is the current active attempt or null. */
    @Synchronized
    fun onTap(attemptId: String?, operation: PurchaseOperation): PurchaseTapAction = when {
        attemptId != null -> PurchaseTapAction.SEND_NOW
        startPending -> PurchaseTapAction.IGNORE
        else -> {
            startPending = true
            pendingOperation = operation
            PurchaseTapAction.START_SERVICE
        }
    }

    /** A new attempt started; records it as the cold service-only purchase attempt. */
    @Synchronized
    fun onAttemptStarted(attemptId: String): Boolean {
        if (!startPending) return false
        startPending = false
        coldAttempt = attemptId
        return true
    }

    /** First accepted /me on the cold attempt: the operation to send now, exactly once. */
    @Synchronized
    fun onVerifiedRights(attemptId: String): PurchaseOperation? {
        if (coldAttempt != attemptId) return null
        return pendingOperation.also { pendingOperation = null }
    }

    @Synchronized
    fun isCold(attemptId: String): Boolean = coldAttempt == attemptId

    /**
     * Stops the cold attempt only when it belongs to this purchase flow; a user-owned
     * attempt (VPN/onboarding) is never stopped by a purchase outcome.
     */
    @Synchronized
    fun stopCold(attemptId: String?): Boolean {
        if (attemptId == null || coldAttempt != attemptId) return false
        clear()
        return true
    }

    /** Bounded timeout for a cold purchase attempt that never delivered its first /me. */
    @Synchronized
    fun onTimeout(attemptId: String?): Boolean {
        if (coldAttempt == null || coldAttempt != attemptId) return false
        clear()
        return true
    }

    /** Control path could not start: clear and surface a fixed error, never a silent no-op. */
    @Synchronized
    fun onControlFailed(): Boolean {
        val failed = startPending || coldAttempt != null
        clear()
        return failed
    }

    @Synchronized
    fun reset() { clear() }

    private fun clear() {
        startPending = false
        coldAttempt = null
        pendingOperation = null
    }

    companion object {
        /** Bounded window for a cold purchase attempt to deliver its first accepted /me. */
        const val COLD_WINDOW_MILLIS = 20_000L

        /** Bounded window to await the post-payment fresh /me before releasing the attempt. */
        const val CONFIRMATION_WINDOW_MILLIS = 20_000L
    }
}

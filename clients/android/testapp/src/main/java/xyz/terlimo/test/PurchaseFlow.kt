package xyz.terlimo.test

import java.time.Instant

/**
 * Bounded host-local purchase flow state for the S5 payment surface. It is a pure
 * projection of accepted server results: [PurchaseFlow.CONFIRMED] is reachable only
 * through a fresh accepted /me projection whose entitlement is active — never from a
 * redirect, a checkout return or a payment status alone. A paid payment stays
 * [PurchaseFlow.AWAITING_CONFIRMATION] while the server has not confirmed the right.
 */
internal data class PurchaseState(
    val phase: String = PurchaseFlow.IDLE,
    val plansRevision: String? = null,
    val plans: List<PaymentPlan> = emptyList(),
    val selectedPlanId: String? = null,
    val selectedMethod: String? = null,
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
    const val UNAVAILABLE = "unavailable"
    const val ERROR = "error"

    /** The purchase action is offered only on the server-owned `purchase_available` signal. */
    fun offered(registration: AccountAccessProjection.Registration?): Boolean =
        registration?.purchaseAvailable == true

    /** Plans the UI may offer: a known duration label and at least one selectable method. */
    fun selectablePlans(plans: List<PaymentPlan>): List<PaymentPlan> = plans.filter { plan ->
        PaymentsText.durationLabel(plan.durationCode) != null &&
            plan.methods.any { PaymentsText.methodLabel(it) != null }
    }

    fun plansLoaded(current: PurchaseState?, revision: String, plans: List<PaymentPlan>): PurchaseState {
        val base = current ?: PurchaseState()
        val selectable = selectablePlans(plans)
        val selected = selectable.firstOrNull { it.planId == base.selectedPlanId } ?: selectable.firstOrNull()
        val method = when {
            selected == null -> null
            base.selectedPlanId == selected.planId && base.selectedMethod in selected.methods -> base.selectedMethod
            else -> selected.methods.firstOrNull()
        }
        return base.copy(
            phase = PLANS,
            plansRevision = revision,
            plans = plans,
            selectedPlanId = selected?.planId,
            selectedMethod = method,
            // A quote stays valid only while the selected plan is unchanged; a different
            // selection must request a new quote instead of silently paying an old one.
            quote = if (base.selectedPlanId == selected?.planId) base.quote else null,
            // The create correlation follows the same attempt: a replaced selection drops it.
            createAck = if (base.selectedPlanId == selected?.planId) base.createAck else null,
            sending = false,
            error = null,
        )
    }

    fun selectPlan(current: PurchaseState?, planId: String): PurchaseState {
        val base = current ?: return PurchaseState()
        val plan = selectablePlans(base.plans).firstOrNull { it.planId == planId } ?: return base
        if (base.selectedPlanId == planId) return base
        return base.copy(
            selectedPlanId = plan.planId,
            selectedMethod = plan.methods.firstOrNull(),
            // A different plan invalidates the previous plan's quote; it is never reused silently.
            quote = null,
            // The replaced attempt's create correlation is dropped with its quote.
            createAck = null,
        )
    }

    fun selectMethod(current: PurchaseState?, method: String): PurchaseState {
        val base = current ?: return PurchaseState()
        val plan = base.plans.firstOrNull { it.planId == base.selectedPlanId } ?: return base
        if (method !in plan.methods || base.selectedMethod == method) return base
        return base.copy(selectedMethod = method, quote = null, createAck = null)
    }

    fun quoteReady(current: PurchaseState?, quote: PaymentQuote): PurchaseState =
        (current ?: PurchaseState()).copy(
            phase = QUOTE_READY, quote = quote, error = null, createAck = null, sending = false)

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
            current.phase !in setOf(QUOTE_READY, AWAITING_PAYMENT) || paidAwaitingBinding(current)) return null
        val quote = current.quote ?: return null
        if (current.selectedPlanId != plan.planId || current.selectedMethod != method ||
            current.plans.firstOrNull { it.planId == plan.planId } != plan || method !in plan.methods ||
            quote.method != method || quote.durationCode != plan.durationCode ||
            quote.amountMinor != plan.amountMinor || quote.currency != plan.currency ||
            quoteExpired(current, now)) return null
        return quote
    }

    /**
     * A quote that expired is a deliberate new attempt: the caller must drop the stale
     * quote and request a new one (with new host-owned keys), never silently reuse it.
     */
    fun quoteExpiredState(current: PurchaseState?): PurchaseState =
        (current ?: PurchaseState()).copy(
            phase = ERROR, quote = null, error = "QUOTE_EXPIRED", createAck = null, sending = false)

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
        return (current ?: PurchaseState()).copy(
            phase = phase, payment = payment, error = error, sending = false)
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
        return if (PaymentsText.isUnavailable(code))
            base.copy(phase = UNAVAILABLE, error = code, createAck = null, sending = false)
        else base.copy(phase = ERROR, error = code, createAck = null, sending = false)
    }

    /** A failed plans refresh cannot leave an older offer or quote actionable. */
    fun plansFailure(current: PurchaseState?, code: String): PurchaseState = failure(
        (current ?: PurchaseState()).copy(
            plansRevision = null, plans = emptyList(), selectedPlanId = null,
            selectedMethod = null, quote = null, payment = null, createAck = null, sending = false,
        ), code)

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
        val creditedRevision = base.payment?.creditedEntitlementRevision ?: return base
        if (projection.entitlement.status != "active" || projection.entitlement.type != "paid" ||
            projection.entitlement.revision != creditedRevision) return base
        return base.copy(phase = CONFIRMED, error = null)
    }

    fun restart(): PurchaseState = PurchaseState()
}

/** The frozen host->native payment actions; the host sends exactly these fields. */
internal sealed class PurchaseOperation {
    object Plans : PurchaseOperation()
    data class Quote(val planId: String, val durationCode: String, val method: String) : PurchaseOperation()
    data class Payment(val quoteId: String) : PurchaseOperation()
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

package xyz.terlimo.test

import java.time.Instant
import java.time.ZoneId
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test

class PurchaseFlowTest {
    private val moscow: ZoneId = ZoneId.of("Europe/Moscow")

    private fun plan(
        planId: String = "p30",
        duration: String = "days:30",
        methods: List<String> = listOf("card", "sbp"),
        amountMinor: Long = 30_000,
    ) = PaymentPlan(planId, "Подписка", duration, 2, amountMinor, "RUB", methods)

    private fun quote(expiresAt: String = "2026-09-19T15:35:00Z") =
        PaymentQuote("q-1", 30_000, "RUB", "days:30", 2, "card", expiresAt)

    private fun payment(status: String, creditedRevision: String? = null) =
        PaymentStatusView("pay-1", status, null, creditedRevision, "not_requested")

    private fun registration(purchaseAvailable: Boolean) =
        AccountAccessProjection.Registration("registered", true, true, null, purchaseAvailable)

    private fun projection(entitlementStatus: String, entitlementType: String = "paid",
                           entitlementRevision: String = "9"): AccountAccessProjection =
        AccountAccessParser.parse(JSONObject(
            """
            {"v":1,"attempt_id":"attempt","type":"account_access","access_version":1,
             "server_time":"2026-09-24T10:00:00Z","session_generation":"2",
             "previous_session_generation":"1","access_revision":"9",
             "account":{"state":"ACTIVE_PAID","telegram_linked":true,"binding_status":"active",
                "management_only":false,"account_ref":"acc-1"},
             "entitlement":{"type":"$entitlementType","status":"$entitlementStatus","valid_from":"2026-09-01T00:00:00Z",
                "valid_until":"2027-01-01T00:00:00Z","effective_device_limit":2,"slots_used":1,
                "revision":"$entitlementRevision","perpetual_commercial":false},
             "onboarding":{"state":"not_started","started_by":"server_confirmed_first_connection",
                "started_at":null,"not_after":null,"duration_seconds":3600,"one_time":true,
                "extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
                "requires_hardware_id":false,"unit":"installation_fingerprint",
                "post_telegram_identity":"account_history_correlation",
                "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
             "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
                "data_access":"subscription_data","effective_deadline":"2027-01-01T00:00:00Z"}}
            """.trimIndent()))

    @Test
    fun plansArriveWithAServerBackedDefaultSelection() {
        val state = PurchaseFlow.plansLoaded(
            PurchaseState(phase = PurchaseFlow.IDLE, selectedPlanId = "p93"),
            "5", listOf(plan(), plan(planId = "p93", duration = "months:3")))
        assertEquals(PurchaseFlow.PLANS, state.phase)
        assertEquals("5", state.plansRevision)
        assertEquals("p93", state.selectedPlanId)
        // The remembered plan keeps its own first method, never a method from another plan.
        assertEquals("months:3", state.plans.first { it.planId == "p93" }.durationCode)
        assertEquals("card", state.selectedMethod)

        val fresh = PurchaseFlow.plansLoaded(null, "5", listOf(plan()))
        assertEquals("p30", fresh.selectedPlanId)
        assertEquals("card", fresh.selectedMethod)
    }

    @Test
    fun changingThePlanDropsTheOldQuoteAndMethodIsValidated() {
        val loaded = PurchaseFlow.plansLoaded(null, "5", listOf(plan(), plan(planId = "p93", duration = "months:3")))
        val quoted = PurchaseFlow.quoteReady(loaded, quote())
        assertEquals(PurchaseFlow.QUOTE_READY, quoted.phase)

        val changed = PurchaseFlow.selectPlan(quoted, "p93")
        assertEquals("p93", changed.selectedPlanId)
        assertNull(changed.quote)

        assertSame(changed, PurchaseFlow.selectMethod(changed, "crypto"))
        val selected = PurchaseFlow.selectMethod(changed, "sbp")
        assertEquals("sbp", selected.selectedMethod)
        val changedMethod = PurchaseFlow.selectMethod(quoted, "sbp")
        assertNull(changedMethod.quote)
    }

    @Test
    fun paidStaysPendingUntilTheFreshMeShowsAnActiveEntitlement() {
        val awaiting = PurchaseFlow.paymentResult(PurchaseState(), payment("paid", "9"))
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION, awaiting.phase)

        // A fresh /me that still does not carry an active right never confirms the purchase.
        val notYet = PurchaseFlow.onFreshMe(awaiting, projection("none"))
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION, notYet.phase)

        // A previously active trial, even after the payment reports paid, is not the
        // newly credited paid right. Nor is an old paid entitlement revision.
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION,
            PurchaseFlow.onFreshMe(awaiting, projection("active", "trial", "9")).phase)
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION,
            PurchaseFlow.onFreshMe(awaiting, projection("active", "paid", "8")).phase)
        assertEquals(PurchaseFlow.AWAITING_CONFIRMATION,
            PurchaseFlow.onFreshMe(PurchaseFlow.paymentResult(PurchaseState(), payment("paid")),
                projection("active", "paid", "9")).phase)

        val confirmed = PurchaseFlow.onFreshMe(awaiting, projection("active"))
        assertEquals(PurchaseFlow.CONFIRMED, confirmed.phase)
        assertNull(confirmed.error)

        // The projection alone is also the only writer of the term line.
        val pendingTerm = SubscriptionTermText.term(projection("active"), awaiting, moscow)
        assertEquals(SubscriptionTermText.AWAITING_CONFIRMATION_TEXT, pendingTerm)
        assertEquals("Подписка действует до 01.01.2027 03:00 (местное время).",
            SubscriptionTermText.term(projection("active"), confirmed, moscow))
    }

    @Test
    fun createdAndPendingStayAwaitingPaymentAndTerminalStatesAreTruthful() {
        assertEquals(PurchaseFlow.AWAITING_PAYMENT,
            PurchaseFlow.paymentResult(PurchaseState(), payment("created")).phase)
        assertEquals(PurchaseFlow.AWAITING_PAYMENT,
            PurchaseFlow.paymentResult(PurchaseState(), payment("pending")).phase)

        listOf("failed", "expired", "refunded", "disputed").forEach { status ->
            val state = PurchaseFlow.paymentResult(PurchaseState(), payment(status))
            assertEquals(PurchaseFlow.ERROR, state.phase)
            assertTrue(PurchaseFlow.terminalPayment(payment(status)))
            assertFalse(PaymentsText.purchaseStatus(state, registration(true), moscow)
                .contains("Подписка обновлена"))
        }
        val failed = PurchaseFlow.paymentResult(PurchaseState(), payment("failed"))
        assertEquals("Платёж не прошёл.", PaymentsText.purchaseStatus(failed, registration(true), moscow))

        val unknown = PurchaseFlow.paymentResult(PurchaseState(), payment("weird"))
        assertEquals(PurchaseFlow.ERROR, unknown.phase)
        assertEquals("Статус оплаты неизвестен.",
            PaymentsText.purchaseStatus(unknown, registration(true), moscow))
    }

    @Test
    fun providerUnavailableAndDisabledPurchaseNeverPretendToSucceed() {
        val unavailable = PurchaseFlow.failure(PurchaseState(phase = PurchaseFlow.PLANS), "PROVIDER_UNAVAILABLE")
        assertEquals(PurchaseFlow.UNAVAILABLE, unavailable.phase)
        assertEquals(PaymentsText.UNAVAILABLE_TEXT,
            PaymentsText.purchaseStatus(PurchaseFlow.failure(PurchaseState(), "SERVICE_UNAVAILABLE"),
                registration(true), moscow))
        assertEquals(PaymentsText.PROVIDER_UNAVAILABLE_TEXT,
            PaymentsText.purchaseStatus(unavailable, registration(true), moscow))

        // purchase_available=false from the accepted /me: no call to action, honest state.
        assertEquals(PaymentsText.UNAVAILABLE_TEXT,
            PaymentsText.purchaseStatus(PurchaseState(), registration(false), moscow))
        assertFalse(PurchaseFlow.offered(registration(false)))
        assertTrue(PurchaseFlow.offered(registration(true)))
    }

    @Test
    fun plansAreOnlyAnOfferAfterServerSuccessAndFailedRefreshClearsOldActions() {
        val idle = PaymentsText.purchaseStatus(PurchaseState(), registration(true), moscow)
        assertEquals(PaymentsText.CHECK_AVAILABILITY_TEXT, idle)
        assertFalse(idle.contains("Покупка доступна"))

        val accepted = PurchaseFlow.plansLoaded(null, "5", listOf(plan()))
        val stale = PurchaseFlow.quoteReady(accepted, quote())
        val failed = PurchaseFlow.plansFailure(stale, "TRANSPORT")
        assertEquals(PurchaseFlow.ERROR, failed.phase)
        assertTrue(failed.plans.isEmpty())
        assertNull(failed.quote)
        assertNull(failed.selectedPlanId)
        assertEquals("Нет связи с сервисом покупки. Повторите позже.",
            PaymentsText.purchaseStatus(failed, registration(true), moscow))
    }

    @Test
    fun expiredQuoteIsDetectedAndDroppedForANewAttempt() {
        val loaded = PurchaseFlow.quoteReady(PurchaseState(), quote("2026-09-19T15:35:00Z"))
        assertFalse(PurchaseFlow.quoteExpired(loaded, Instant.parse("2026-09-19T15:34:59Z")))
        assertTrue(PurchaseFlow.quoteExpired(loaded, Instant.parse("2026-09-19T15:35:01Z")))
        val expired = PurchaseFlow.quoteExpiredState(loaded)
        assertNull(expired.quote)
        assertEquals(PurchaseFlow.ERROR, expired.phase)
        assertEquals("QUOTE_EXPIRED", expired.error)
        assertEquals("Предложение истекло. Оформите его заново.",
            PaymentsText.purchaseStatus(expired, registration(true), moscow))
    }

    @Test
    fun explicitGateStartsOneColdAttemptAndDeliversTheOperationOnce() {
        val gate = PurchaseGate()
        assertEquals(PurchaseTapAction.START_SERVICE, gate.onTap(null, PurchaseOperation.Plans))
        // A repeated tap while the cold attempt is starting is coalesced, not duplicated.
        assertEquals(PurchaseTapAction.IGNORE, gate.onTap(null, PurchaseOperation.Plans))
        assertTrue(gate.onAttemptStarted("attempt-1"))
        assertTrue(gate.isCold("attempt-1"))
        assertSame(PurchaseOperation.Plans, gate.onVerifiedRights("attempt-1"))
        // Delivered exactly once: the pending operation is cleared.
        assertNull(gate.onVerifiedRights("attempt-1"))
        // A live attempt takes the direct path; only the cold attempt is releasable here.
        assertEquals(PurchaseTapAction.SEND_NOW, gate.onTap("attempt-2", PurchaseOperation.Plans))
        assertFalse(gate.stopCold("attempt-2"))
        assertFalse(gate.stopCold(null))
        assertTrue(gate.stopCold("attempt-1"))
        assertFalse(gate.isCold("attempt-1"))
    }

    @Test
    fun coldGateTimeoutAndControlFailureClearThePendingOperation() {
        val timeout = PurchaseGate()
        assertEquals(PurchaseTapAction.START_SERVICE, timeout.onTap(null, PurchaseOperation.Plans))
        assertTrue(timeout.onAttemptStarted("a"))
        assertTrue(timeout.onTimeout("a"))
        assertNull(timeout.onVerifiedRights("a"))

        val failed = PurchaseGate()
        assertEquals(PurchaseTapAction.START_SERVICE, failed.onTap(null, PurchaseOperation.Plans))
        assertTrue(failed.onControlFailed())
        assertFalse(failed.onControlFailed())
        assertFalse(failed.onAttemptStarted("b"))
    }

    @Test
    fun noPlanOrMethodSelectionStillExposesSafeTexts() {
        val loaded = PurchaseFlow.plansLoaded(null, "5", emptyList())
        assertEquals(PurchaseFlow.PLANS, loaded.phase)
        assertEquals("Тарифы не загружены.", PaymentsText.purchaseStatus(loaded, registration(true), moscow))
        assertNotNull(PaymentsText.purchaseStatus(null, null, moscow))
        assertTrue(PurchaseFlow.selectablePlans(listOf(plan(duration = "days:7"))).isEmpty())
        assertTrue(PurchaseFlow.selectablePlans(listOf(plan(methods = emptyList()))).isEmpty())
    }
}

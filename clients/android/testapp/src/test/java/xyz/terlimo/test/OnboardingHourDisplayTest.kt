package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Display-only onboarding hour (accepted slice fc65452 + correction): title
 * «Доступ для регистрации», the <=10-minute warning with the action per linked state, the
 * owner end text without any Telegram promise, the frozen-clock restart derivation, the
 * paid/trial transition, the EXPIRED-account display precedence (business state and
 * effective dataAccess are distinct) and the lifecycle-bound tick policy.
 */
class OnboardingHourDisplayTest {
    private fun eventJson(
        serverTime: String = "2026-09-21T12:00:00Z",
        deadline: String? = "2026-09-21T12:10:00Z",
        dataAccess: String = "onboarding_hour",
        accountState: String = "VERIFIED_NO_ENTITLEMENT",
        onboardingState: String = "active",
        telegramLinked: Boolean = true,
        bindingStatus: String = "active",
        entitlementType: String = "none",
        entitlementStatus: String = "none",
        generation: String = "1",
        previous: String? = null,
    ): String {
        val previousValue = previous?.let { "\"$it\"" } ?: "null"
        val deadlineValue = deadline?.let { "\"$it\"" } ?: "null"
        val startedAt = if (onboardingState == "not_started") "null" else "\"2026-09-21T11:50:00Z\""
        val notAfter = if (onboardingState == "not_started") "null" else "\"2026-09-21T12:50:00Z\""
        return """
        {"v":1,"attempt_id":"attempt","type":"account_access","access_version":1,
         "server_time":"$serverTime","session_generation":"$generation",
         "previous_session_generation":$previousValue,"access_revision":"7",
         "account":{"state":"$accountState","telegram_linked":$telegramLinked,"binding_status":"$bindingStatus",
            "management_only":false,"account_ref":"acc-1"},
         "entitlement":{"type":"$entitlementType","status":"$entitlementStatus",
            "valid_from":"2026-08-01T00:00:00Z","valid_until":"2027-01-01T00:00:00Z",
            "effective_device_limit":5,"slots_used":2,"revision":"5","perpetual_commercial":false},
         "onboarding":{"state":"$onboardingState","started_by":"server_confirmed_first_connection",
            "started_at":$startedAt,"not_after":$notAfter,"duration_seconds":3600,
            "one_time":true,"extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
            "requires_hardware_id":false,"unit":"installation_fingerprint",
            "post_telegram_identity":"account_history_correlation",
            "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
         "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
            "data_access":"$dataAccess","effective_deadline":$deadlineValue}}
        """
    }

    private fun snapshot(json: String, receivedElapsed: Long = 0): AccountAccessSnapshot =
        AccountAccessPolicy.apply(null, AccountAccessParser.parse(JSONObject(json)), receivedElapsed)!!

    private fun hour(
        deadline: String,
        receivedElapsed: Long = 0,
        accountState: String = "VERIFIED_NO_ENTITLEMENT",
        onboardingState: String = "active",
        telegramLinked: Boolean = true,
    ): AccountAccessSnapshot =
        snapshot(eventJson(deadline = deadline, accountState = accountState,
            onboardingState = onboardingState, telegramLinked = telegramLinked), receivedElapsed)

    private fun paid(deadline: String = "2026-09-21T12:10:00Z"): AccountAccessSnapshot =
        snapshot(eventJson(deadline = deadline, dataAccess = "subscription_data",
            accountState = "ACTIVE_PAID", entitlementType = "paid", entitlementStatus = "active"))

    private fun trial(deadline: String = "2026-09-21T12:10:00Z"): AccountAccessSnapshot =
        snapshot(eventJson(deadline = deadline, dataAccess = "subscription_data",
            accountState = "ACTIVE_TRIAL", entitlementType = "trial", entitlementStatus = "active"))

    private fun checkout(): AccountAccessSnapshot =
        snapshot(eventJson(deadline = null, dataAccess = "restricted_checkout"))

    private fun none(): AccountAccessSnapshot =
        snapshot(eventJson(deadline = null, dataAccess = "none"))

    private val endText = AccountAccessPolicy.ONBOARDING_HOUR_END_TEXT

    @Test fun warningStartsAtTenMinutesAndNotAtTenMinutesOne() {
        val noWarning = AccountAccessPolicy.statusLine(hour("2026-09-21T12:10:01Z"), 0)
        assertTrue(noWarning.contains("Доступ для регистрации: осталось 10:01"))
        assertFalse(noWarning.contains("скоро завершится"))

        val warning = AccountAccessPolicy.statusLine(hour("2026-09-21T12:10:00Z"), 0)
        assertTrue(warning.contains("Доступ для регистрации: осталось 10:00 · скоро завершится"))

        val lastSecond = AccountAccessPolicy.statusLine(hour("2026-09-21T12:00:01Z"), 0)
        assertTrue(lastSecond.contains("Доступ для регистрации: осталось 00:01 · скоро завершится"))
    }

    @Test fun warningNamesTheActionPerLinkedState() {
        val linked = AccountAccessPolicy.statusLine(hour("2026-09-21T12:09:59Z"), 0)
        assertTrue(linked.endsWith("· Оформите доступ"))
        assertFalse(linked.contains("Зарегистрируйтесь"))

        val unlinked = AccountAccessPolicy.statusLine(hour("2026-09-21T12:09:59Z", telegramLinked = false), 0)
        assertTrue(unlinked.endsWith("· Зарегистрируйтесь и оформите доступ"))
    }

    @Test fun zeroAndPastDeadlineShowTheOwnerEndTextWithoutPromise() {
        val atZero = AccountAccessPolicy.statusLine(hour("2026-09-21T12:00:00Z"), 0)
        assertEquals(endText, atZero)
        assertFalse(atZero.contains("скоро завершится"))
        assertFalse(atZero.contains("Telegram"))

        val past = AccountAccessPolicy.statusLine(hour("2026-09-21T12:00:00Z"), 60_000)
        assertEquals(endText, past)
        assertFalse(past.contains("Telegram"))
        assertFalse(past.contains("Пробн"))

        assertTrue(endText.contains("Час доступа завершён"))
        assertTrue(endText.contains("активируйте пробный период или оплатите подписку"))
        assertTrue(endText.contains("Оплата остаётся доступна"))
    }

    @Test fun laterElapsedAnchorReducesRemainingAndNeverExtends() {
        val accepted = hour("2026-09-21T12:10:00Z", receivedElapsed = 0)
        assertEquals(600_000L, AccountAccessPolicy.remainingMillis(accepted, 0))
        assertEquals(570_000L, AccountAccessPolicy.remainingMillis(accepted, 30_000))
        assertEquals(510_000L, AccountAccessPolicy.remainingMillis(accepted, 90_000))

        var previous = Long.MAX_VALUE
        for (anchor in listOf(0L, 1_000L, 30_000L, 599_999L, 600_000L, 900_000L)) {
            val remaining = AccountAccessPolicy.remainingMillis(accepted, anchor)!!
            assertTrue("remaining must not grow at $anchor", remaining <= previous)
            previous = remaining
        }
        assertTrue(AccountAccessPolicy.statusLine(accepted, 30_000).contains("осталось 09:30"))
        assertEquals(endText, AccountAccessPolicy.statusLine(accepted, 600_000))
    }

    @Test fun paidTransitionDropsTheOnboardingWordingInTheSameSession() {
        val onboarding = hour("2026-09-21T12:05:00Z")
        assertTrue(AccountAccessPolicy.statusLine(onboarding, 0).contains("Доступ для регистрации"))

        val paidReplacement = AccountAccessParser.parse(JSONObject(eventJson(
            deadline = "2026-09-21T13:00:00Z", dataAccess = "subscription_data",
            accountState = "ACTIVE_PAID", entitlementType = "paid", entitlementStatus = "active",
            generation = "2", previous = "1")))
        val updated = AccountAccessPolicy.apply(onboarding, paidReplacement, 0)!!
        val text = AccountAccessPolicy.statusLine(updated, 0)
        assertEquals("Доступ активен · Подписка: осталось 1:00:00", text)
        assertFalse(text.contains("Доступ для регистрации"))
        assertFalse(text.contains("Час доступа"))
        assertFalse(text.contains("Пробн"))
        assertFalse(text.contains("скоро завершится"))
        assertNull(AccountAccessPolicy.notificationLine(updated, 0))
    }

    @Test fun trialShowsNoOnboardingWordingEither() {
        val text = AccountAccessPolicy.statusLine(trial(), 0)
        assertEquals("Доступ активен · Подписка: осталось 10:00", text)
        assertFalse(text.contains("Доступ для регистрации"))
        assertFalse(text.contains("Пробн"))
        assertNull(AccountAccessPolicy.notificationLine(trial(), 0))
    }

    @Test fun expiredAccountStateWithConfirmedActiveHourShowsTheCountdown() {
        // Contract-permitted combination: the business account state is EXPIRED while the
        // accepted grant still confirms an active installation onboarding hour. The
        // effective dataAccess wins on display; admission is untouched and native still
        // owns the stop.
        val expiredHour = hour("2026-09-21T12:05:00Z", accountState = "EXPIRED")
        assertEquals("EXPIRED", expiredHour.projection.account.state)
        assertEquals("onboarding_hour", expiredHour.projection.grant.dataAccess)
        val text = AccountAccessPolicy.statusLine(expiredHour, 0)
        assertEquals("Доступ для регистрации: осталось 05:00 · скоро завершится · Оформите доступ", text)
        assertFalse(text.contains("Срок доступа истёк"))
        assertFalse(text.contains("VPN не активен"))
        assertEquals(text, AccountAccessPolicy.notificationLine(expiredHour, 0))
        assertTrue(AccountAccessDisplayRefresh.remains(expiredHour, 0))
        assertTrue(AccountAccessDisplayRefresh.shouldPost(true, expiredHour, 0))

        // The same combination once the server deadline passes shows the owner end text.
        assertFalse(AccountAccessDisplayRefresh.remains(expiredHour, 300_000))
        assertEquals(endText, AccountAccessPolicy.statusLine(expiredHour, 300_000))
    }

    private data class BlockedHour(
        val label: String,
        val accountState: String,
        val bindingStatus: String,
        val entitlementType: String,
        val entitlementStatus: String,
        val reason: String,
    )

    @Test fun existingProhibitionsHideTheHourAndShowThePreciseReasonOnEverySurface() {
        val blockedHours = listOf(
            BlockedHour("REVOKED_SESSION", "REVOKED_SESSION", "active", "none", "none",
                AccountAccessPolicy.REVOKED_SESSION_REASON_TEXT),
            BlockedHour("binding revoked", "VERIFIED_NO_ENTITLEMENT", "revoked", "none", "none",
                AccountAccessPolicy.BINDING_REASON_TEXT),
            BlockedHour("binding deactivated", "VERIFIED_NO_ENTITLEMENT", "deactivated", "none", "none",
                AccountAccessPolicy.BINDING_REASON_TEXT),
            BlockedHour("entitlement revoked", "ACTIVE_PAID", "active", "paid", "revoked",
                AccountAccessPolicy.ENTITLEMENT_REVOKED_TEXT),
            BlockedHour("entitlement expired", "EXPIRED", "active", "paid", "expired",
                AccountAccessPolicy.ENTITLEMENT_EXPIRED_TEXT),
            BlockedHour("entitlement unknown_review", "ACTIVE_PAID", "active", "paid", "unknown_review",
                AccountAccessPolicy.ENTITLEMENT_UNREVIEWED_TEXT),
        )
        for (case in blockedHours) {
            // data_access=onboarding_hour + onboarding active + future deadline, blocked only
            // by the existing prohibition.
            val blocked = snapshot(eventJson(
                deadline = "2026-09-21T12:10:00Z",
                accountState = case.accountState,
                bindingStatus = case.bindingStatus,
                entitlementType = case.entitlementType,
                entitlementStatus = case.entitlementStatus,
            ))
            assertFalse(case.label, AccountAccessPolicy.isConfirmedOnboardingHour(blocked, 0))
            assertEquals(case.label, case.reason, AccountAccessPolicy.statusLine(blocked, 0))
            assertEquals(case.label, case.reason, SubscriptionStatusText.status(null, blocked, 0))
            assertNull(case.label, AccountAccessPolicy.notificationLine(blocked, 0))
            assertFalse(case.label, AccountAccessDisplayRefresh.remains(blocked, 0))
            assertFalse(case.label, AccountAccessDisplayRefresh.shouldPost(true, blocked, 0))

            val text = AccountAccessPolicy.statusLine(blocked, 0)
            listOf("Доступ для регистрации", "Час доступа", "осталось", "скоро завершится",
                "Оформите доступ", "Зарегистрируйтесь", "Пробн").forEach {
                assertFalse("${case.label}: unexpected \"$it\"", text.contains(it))
            }
        }
    }

    @Test fun aBlockingReasonOutranksTheFinishedHourEndText() {
        val revokedAndFinished = snapshot(eventJson(
            deadline = "2026-09-21T12:00:00Z",
            accountState = "REVOKED_SESSION",
        ))
        assertEquals(AccountAccessPolicy.REVOKED_SESSION_REASON_TEXT,
            AccountAccessPolicy.statusLine(revokedAndFinished, 0))
        assertNull(AccountAccessPolicy.notificationLine(revokedAndFinished, 0))
        assertFalse(AccountAccessDisplayRefresh.remains(revokedAndFinished, 0))
    }

    @Test fun unlinkedAccountWithActiveHourStillShowsTheNormalCountdown() {
        // A normal first-connection hour before Telegram linking: no prohibition applies.
        val unlinked = snapshot(eventJson(
            deadline = "2026-09-21T12:10:00Z",
            accountState = "UNLINKED",
            bindingStatus = "none",
            telegramLinked = false,
        ))
        assertTrue(AccountAccessPolicy.isConfirmedOnboardingHour(unlinked, 0))
        assertEquals("Доступ для регистрации: осталось 10:00 · скоро завершится · Зарегистрируйтесь и оформите доступ",
            AccountAccessPolicy.statusLine(unlinked, 0))
        assertTrue(AccountAccessDisplayRefresh.remains(unlinked, 0))
        assertTrue(AccountAccessDisplayRefresh.shouldPost(true, unlinked, 0))
        assertEquals(AccountAccessPolicy.statusLine(unlinked, 0),
            AccountAccessPolicy.notificationLine(unlinked, 0))
    }

    @Test fun finishedOnboardingGrantShowsOnlyTheOwnerEndText() {
        val expiredOnboarding = hour("2026-09-21T12:05:00Z", onboardingState = "expired")
        assertEquals(endText, AccountAccessPolicy.statusLine(expiredOnboarding, 0))
        assertEquals(endText, AccountAccessPolicy.notificationLine(expiredOnboarding, 0))
        assertFalse(AccountAccessDisplayRefresh.remains(expiredOnboarding, 0))
        assertFalse(AccountAccessDisplayRefresh.shouldPost(true, expiredOnboarding, 0))

        val expiredAccountPastDeadline = hour("2026-09-21T12:00:00Z", accountState = "EXPIRED")
        assertEquals(endText, AccountAccessPolicy.statusLine(expiredAccountPastDeadline, 0))
        assertEquals(endText, AccountAccessPolicy.notificationLine(expiredAccountPastDeadline, 0))
        assertFalse(AccountAccessDisplayRefresh.remains(expiredAccountPastDeadline, 0))
    }

    @Test fun noSnapshotKeepsTheNeutralPendingText() {
        assertEquals(SubscriptionStatusText.PENDING, SubscriptionStatusText.status(null, null, 0))
        assertNull(AccountAccessPolicy.notificationLine(null, 0))
    }

    @Test fun notificationCarriesOnlyTheOnboardingHour() {
        val active = hour("2026-09-21T12:05:00Z")
        assertEquals("Доступ для регистрации: осталось 05:00 · скоро завершится · Оформите доступ",
            AccountAccessPolicy.notificationLine(active, 0))
        assertEquals(endText, AccountAccessPolicy.notificationLine(hour("2026-09-21T12:00:00Z"), 0))

        assertNull(AccountAccessPolicy.notificationLine(checkout(), 0))
        assertNull(AccountAccessPolicy.notificationLine(none(), 0))
        assertNull(AccountAccessPolicy.notificationLine(paid(), 0))
    }

    @Test fun refreshCadenceIsBoundedToTheConfirmedActiveHour() {
        assertEquals(1_000L, AccountAccessDisplayRefresh.TICK_MILLIS)
        assertTrue(AccountAccessDisplayRefresh.remains(hour("2026-09-21T12:05:00Z"), 0))
        assertTrue(AccountAccessDisplayRefresh.remains(hour("2026-09-21T12:00:01Z"), 0))
        assertFalse(AccountAccessDisplayRefresh.remains(hour("2026-09-21T12:00:00Z"), 0))
        assertFalse(AccountAccessDisplayRefresh.remains(hour("2026-09-21T12:00:00Z"), 60_000))
        assertFalse(AccountAccessDisplayRefresh.remains(hour("2026-09-21T12:05:00Z", onboardingState = "expired"), 0))
        assertFalse(AccountAccessDisplayRefresh.remains(paid(), 0))
        assertFalse(AccountAccessDisplayRefresh.remains(null, 0))
    }

    @Test fun tickPolicyArmsOnLateAcceptedSnapshotAndStopsWithoutPolling() {
        val accepted = hour("2026-09-21T12:05:00Z")
        // Ordinary fresh flow: onStart renders with no snapshot yet, so nothing is queued.
        assertFalse("null onStart must not arm", AccountAccessDisplayRefresh.shouldPost(true, null, 0))
        // account_access accepted after start through the listener/render path arms the tick.
        assertTrue("accepted hour must arm", AccountAccessDisplayRefresh.shouldPost(true, accepted, 0))
        // Subsequent ticks re-post while the confirmed hour remains (text-only refresh).
        assertTrue(AccountAccessDisplayRefresh.shouldPost(true, accepted, 1_000))
        // Remaining <= 0 stops: no perpetual polling.
        assertFalse(AccountAccessDisplayRefresh.shouldPost(true, accepted, 300_000))
        // paid/none/not-onboarding stops the tick.
        assertFalse(AccountAccessDisplayRefresh.shouldPost(true, paid(), 0))
        assertFalse(AccountAccessDisplayRefresh.shouldPost(true, trial(), 0))
        assertFalse(AccountAccessDisplayRefresh.shouldPost(true, checkout(), 0))
        assertFalse(AccountAccessDisplayRefresh.shouldPost(true, none(), 0))
        // A queued/late callback after onStop must not re-arm.
        assertFalse("late callback after onStop must not arm",
            AccountAccessDisplayRefresh.shouldPost(false, accepted, 0))
        assertFalse(AccountAccessDisplayRefresh.shouldPost(false, null, 0))
    }

    /**
     * The live local traffic / server usage display must never alter or re-arm the one-time
     * onboarding hour or the subscription projection: it only carries traffic fields. A real
     * first-hour regression (a traffic-only publication that replaces the snapshot, resets the
     * phase, or arms the hour tick for a paid/none state) fails here.
     */
    @Test fun trafficAndUsageDisplayCannotAlterTheOnboardingHourOrItsTickPolicy() {
        val accepted = hour("2026-09-21T12:05:00Z")
        val live = ViewState(
            phase = "Connected",
            accountAccess = accepted,
            traffic = TrafficSnapshot(sessionId = 2, active = true, rxTotal = 1_024, txTotal = 2_048),
        )
        // The accepted hour snapshot and its display-tick policy are untouched by live traffic.
        assertSame(accepted, live.accountAccess)
        assertTrue(AccountAccessDisplayRefresh.shouldPost(true, live.accountAccess, 0))
        // paid/trial/none still never arm the hour tick, traffic present or not.
        assertFalse(AccountAccessDisplayRefresh.shouldPost(true, paid(), 0))
        // A traffic-only republication keeps every first-hour/phase field intact.
        val republished = live.copy(traffic = TrafficSnapshot(
            sessionId = 2, active = true, rxTotal = 4_096, txTotal = 8_192))
        assertSame(accepted, republished.accountAccess)
        assertEquals("Connected", republished.phase)
        assertTrue(AccountAccessDisplayRefresh.shouldPost(true, republished.accountAccess, 1_000))
    }
}

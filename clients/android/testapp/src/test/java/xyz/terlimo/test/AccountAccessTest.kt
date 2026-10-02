package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

class AccountAccessTest {
    private fun eventJson(
        generation: String = "1",
        previous: String? = null,
        revision: String = "7",
        serverTime: String = "2026-09-21T12:00:00Z",
        deadline: String? = "2026-09-21T12:10:00Z",
        dataAccess: String = "subscription_data",
        onboardingState: String = "active",
        perpetualCommercial: Boolean = false,
        validUntil: String? = "2027-01-01T00:00:00Z",
        extraTopLevel: String = "",
    ): String {
        val previousValue = previous?.let { "\"$it\"" } ?: "null"
        val deadlineValue = deadline?.let { "\"$it\"" } ?: "null"
        val validUntilValue = validUntil?.let { "\"$it\"" } ?: "null"
        val startedAt = if (onboardingState == "not_started") "null" else "\"2026-09-21T10:00:00Z\""
        val notAfter = if (onboardingState == "not_started") "null" else "\"2026-09-21T11:00:00Z\""
        return """
        {"v":1,"attempt_id":"attempt","type":"account_access","access_version":1,
         "server_time":"$serverTime","session_generation":"$generation",
         "previous_session_generation":$previousValue,"access_revision":"$revision",
         "account":{"state":"ACTIVE_PAID","telegram_linked":true,"binding_status":"active",
            "management_only":false,"account_ref":"acc-1"},
         "entitlement":{"type":"paid","status":"active","valid_from":"2026-08-01T00:00:00Z",
            "valid_until":$validUntilValue,"effective_device_limit":5,"slots_used":2,
            "revision":"5","perpetual_commercial":$perpetualCommercial},
         "onboarding":{"state":"$onboardingState","started_by":"server_confirmed_first_connection",
            "started_at":$startedAt,"not_after":$notAfter,"duration_seconds":3600,"one_time":true,
            "extends_on_refresh":false,"extends_on_restart":false,"creates_trial":false,
            "requires_hardware_id":false,"unit":"installation_fingerprint",
            "post_telegram_identity":"account_history_correlation",
            "pre_telegram_reinstall":"may_be_indistinguishable_new_key_separate_unit"},
         "grant_resolution":{"control_available":true,"restricted_checkout_available":true,
            "data_access":"$dataAccess","effective_deadline":$deadlineValue}$extraTopLevel}
        """
    }

    /**
     * Frozen bridge projection of the accepted indefinite /me shape at 3859e43
     * (go_client/accountaccess/projection.go buildProjection of the indefinite fixture in
     * accountaccess/indefinite_test.go): perpetual commercial entitlement, valid_until
     * omitted because the source carries none, subscription_data with a null deadline.
     */
    private fun indefiniteEventJson(): String {
        val event = JSONObject(eventJson(
            deadline = null, dataAccess = "subscription_data", onboardingState = "not_started",
            perpetualCommercial = true, validUntil = "2027-01-01T00:00:00Z"))
        event.getJSONObject("entitlement").remove("valid_until")
        return event.toString()
    }

    @Test
    fun parseAcceptsFrozenSchema() {
        val projection = AccountAccessParser.parse(JSONObject(eventJson()))
        assertEquals("1", projection.sessionGeneration)
        assertNull(projection.previousSessionGeneration)
        assertEquals("7", projection.accessRevision)
        assertEquals("ACTIVE_PAID", projection.account.state)
        assertEquals("subscription_data", projection.grant.dataAccess)
        assertEquals("2026-09-21T12:10:00Z", projection.grant.effectiveDeadline)
        val onboardingFree = AccountAccessParser.parse(
            JSONObject(eventJson(dataAccess = "restricted_checkout", deadline = null, onboardingState = "not_started")))
        assertEquals("not_started", onboardingFree.onboarding.state)
        assertNull(onboardingFree.grant.effectiveDeadline)
    }

    @Test
    fun parseRejectsAdditiveMissingAndNonCanonicalFields() {
        assertThrows(IllegalStateException::class.java) {
            AccountAccessParser.parse(JSONObject(eventJson(extraTopLevel = ",\"additive\":1")))
        }
        assertThrows(IllegalStateException::class.java) {
            val missing = JSONObject(eventJson())
            missing.remove("onboarding")
            AccountAccessParser.parse(missing)
        }
        assertThrows(IllegalStateException::class.java) {
            AccountAccessParser.parse(JSONObject(eventJson(serverTime = "2026-09-21T12:00:00+00:00")))
        }
        assertThrows(IllegalStateException::class.java) {
            AccountAccessParser.parse(JSONObject(eventJson(dataAccess = "none")))
        }
        assertThrows(IllegalStateException::class.java) {
            val inconsistent = JSONObject(eventJson())
            inconsistent.getJSONObject("onboarding").put("state", "not_started")
            AccountAccessParser.parse(inconsistent)
        }
        assertThrows(IllegalStateException::class.java) {
            val versionTwo = JSONObject(eventJson())
            versionTwo.put("access_version", 2)
            AccountAccessParser.parse(versionTwo)
        }
    }

    @Test
    fun chainAcceptsLinkedReplacementsAndRejectsNonLinkingEvents() {
        val first = AccountAccessPolicy.accept(AccountAccessChain(), "A", null)
        assertNotNull(first)
        assertEquals("A", first!!.currentGen)

        val second = AccountAccessPolicy.accept(first, "B", "A")
        assertNotNull(second)
        assertEquals(AccountAccessChain("B", "A"), second)

        val third = AccountAccessPolicy.accept(second!!, "C", "B")
        assertEquals(AccountAccessChain("C", "B"), third)

        // Same pair repeats are accepted; anything else is ignored and the pair is bounded.
        assertEquals(AccountAccessChain("C", "B"), AccountAccessPolicy.accept(third!!, "C", "B"))
        assertNull(AccountAccessPolicy.accept(third, "A", null))
        assertNull(AccountAccessPolicy.accept(third, "B", "A"))
        assertNull(AccountAccessPolicy.accept(third, "C", "A"))
        assertNull(AccountAccessPolicy.accept(third, "D", "A"))
    }

    @Test
    fun firstEventMustBeUnlinked() {
        assertNotNull(AccountAccessPolicy.accept(AccountAccessChain(), "A", null))
        assertNull(AccountAccessPolicy.accept(AccountAccessChain(), "A", "X"))
    }

    @Test
    fun rejectedEventKeepsLastGoodSnapshot() {
        val projection = AccountAccessParser.parse(JSONObject(eventJson()))
        val accepted = AccountAccessPolicy.apply(null, projection, 1_000)!!
        val nonLinking = AccountAccessParser.parse(JSONObject(eventJson(generation = "2")))
        assertNull(AccountAccessPolicy.apply(accepted, nonLinking, 2_000))
        assertEquals(1_000, accepted.receivedElapsed)
    }

    @Test
    fun uiClockAnchorsAtReceiptAndNeverUsesTheWallClock() {
        val projection = AccountAccessParser.parse(JSONObject(eventJson()))
        val snapshot = AccountAccessPolicy.apply(null, projection, 50_000)!!
        assertEquals(600_000L, AccountAccessPolicy.remainingMillis(snapshot, 50_000))
        assertEquals(500_000L, AccountAccessPolicy.remainingMillis(snapshot, 150_000))
        assertEquals(1L, AccountAccessPolicy.remainingMillis(snapshot, 649_999))
        assertEquals(0L, AccountAccessPolicy.remainingMillis(snapshot, 650_000))
        assertEquals(0L, AccountAccessPolicy.remainingMillis(snapshot, 10_000_000))
    }

    @Test
    fun statusLineUsesTheFrozenProjectionClock() {
        val subscription = AccountAccessParser.parse(JSONObject(eventJson()))
        val snapshot = AccountAccessPolicy.apply(null, subscription, 50_000)!!
        assertEquals("Доступ активен · Подписка: осталось 10:00",
            AccountAccessPolicy.statusLine(snapshot, 50_000))

        val hour = AccountAccessParser.parse(
            JSONObject(eventJson(dataAccess = "onboarding_hour", deadline = "2026-09-21T12:05:00Z")))
        val hourSnapshot = AccountAccessPolicy.apply(null, hour, 0)!!
        assertEquals("Доступ для регистрации: осталось 05:00 · скоро завершится · Оформите доступ",
            AccountAccessPolicy.statusLine(hourSnapshot, 0))

        val checkout = AccountAccessParser.parse(
            JSONObject(eventJson(dataAccess = "restricted_checkout", deadline = null)))
        val checkoutSnapshot = AccountAccessPolicy.apply(null, checkout, 0)!!
        assertEquals("Доступ активен · VPN не активен; доступна оплата",
            AccountAccessPolicy.statusLine(checkoutSnapshot, 0))

        val expired = JSONObject(eventJson()).put("account",
            JSONObject(eventJson()).getJSONObject("account").put("state", "EXPIRED"))
        val expiredSnapshot = AccountAccessPolicy.apply(null, AccountAccessParser.parse(expired), 0)!!
        assertEquals("Срок доступа истёк · Подписка: осталось 10:00",
            AccountAccessPolicy.statusLine(expiredSnapshot, 0))
    }

    @Test
    fun nullDeadlineHasNoCountdown() {
        val projection = AccountAccessParser.parse(
            JSONObject(eventJson(dataAccess = "restricted_checkout", deadline = null)))
        val snapshot = AccountAccessPolicy.apply(null, projection, 0)!!
        assertNull(AccountAccessPolicy.remainingMillis(snapshot, 0))
    }

    @Test
    fun indefiniteSubscriptionDataMirrorsTheGoTrustedPredicate() {
        // Positive: the perpetual commercial entitlement without valid_until carries a null
        // deadline; the verified catalog validity and node lease remain the finite bounds.
        val indefinite = AccountAccessParser.parse(JSONObject(indefiniteEventJson()))
        assertEquals("subscription_data", indefinite.grant.dataAccess)
        assertNull(indefinite.grant.effectiveDeadline)
        assertTrue(indefinite.entitlement.perpetualCommercial)
        assertNull(indefinite.entitlement.validUntil)

        // Positive: a finite deadline is accepted for every entitlement shape.
        val finitePerpetual = AccountAccessParser.parse(JSONObject(
            eventJson(perpetualCommercial = true, validUntil = null)))
        assertEquals("2026-09-21T12:10:00Z", finitePerpetual.grant.effectiveDeadline)

        // Negative: every null deadline outside the rule stays malformed and fail-closed.
        assertThrows(IllegalStateException::class.java) {
            AccountAccessParser.parse(JSONObject(eventJson(
                deadline = null, dataAccess = "subscription_data", perpetualCommercial = false)))
        }
        assertThrows(IllegalStateException::class.java) {
            AccountAccessParser.parse(JSONObject(eventJson(
                deadline = null, dataAccess = "subscription_data", perpetualCommercial = true,
                validUntil = "2027-01-01T00:00:00Z")))
        }
        assertThrows(IllegalStateException::class.java) {
            AccountAccessParser.parse(JSONObject(eventJson(
                deadline = null, dataAccess = "onboarding_hour", perpetualCommercial = true, validUntil = null)))
        }
        assertThrows(IllegalStateException::class.java) {
            AccountAccessParser.parse(JSONObject(eventJson(
                deadline = "later", dataAccess = "subscription_data", perpetualCommercial = true, validUntil = null)))
        }
    }

    @Test
    fun indefiniteGrantHasNoCountdownAndNoImpliedRevocation() {
        val projection = AccountAccessParser.parse(JSONObject(indefiniteEventJson()))
        val snapshot = AccountAccessPolicy.apply(null, projection, 50_000)!!
        assertNull(AccountAccessPolicy.remainingMillis(snapshot, 50_000))
        assertEquals("Доступ активен · Подписка активна", AccountAccessPolicy.statusLine(snapshot, 50_000))
        // The same generation pair is a repeat, a non-linking replacement is still ignored.
        assertNotNull(AccountAccessPolicy.accept(snapshot.chain, "1", null))
        assertNull(AccountAccessPolicy.accept(snapshot.chain, "2", null))
    }
    @Test fun deviceCountsAcceptInt32WithoutCommercialCapAndRejectLossyNumbers() {
        for (limit in listOf(0, 101, Int.MAX_VALUE)) {
            val event = JSONObject(eventJson())
            val counts = event.getJSONObject("entitlement")
            counts.put("effective_device_limit", limit).put("slots_used", Int.MAX_VALUE)
            val parsed = AccountAccessParser.parse(event)
            assertEquals(limit, parsed.entitlement.effectiveDeviceLimit)
        }
        for (field in listOf("effective_device_limit", "slots_used")) {
            for (bad in listOf(-1, 1.5, 1.0, Int.MAX_VALUE.toLong() + 1, Long.MAX_VALUE, "101", JSONObject.NULL)) {
                val event = JSONObject(eventJson())
                (event.getJSONObject("entitlement")).put(field, bad)
                assertThrows(IllegalStateException::class.java) { AccountAccessParser.parse(event) }
            }
        }
    }

}

package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import java.io.File

class ReferralContractTest {
    private fun fixtures(): JSONObject {
        val root = generateSequence(File(System.getProperty("user.dir"))) { it.parentFile }
            .first { File(it, "docs/TERLIMO_IMPLEMENTATION/referral_20261003/native-wire-fixtures.json").isFile }
        return JSONObject(File(root, "docs/TERLIMO_IMPLEMENTATION/referral_20261003/native-wire-fixtures.json").readText())
    }
    private fun info(): JSONObject = fixtures().getJSONObject("referralInfoFixture")
    // Byte-identical copy of published ba3934f registration-wire-fixtures.json.
    private fun registrationFixtures(): JSONObject = JSONObject(checkNotNull(
        javaClass.getResourceAsStream("/referral-registration-wire-fixtures.json")
    ).bufferedReader().use { it.readText() })

    @Test fun `published native fixtures use the exact server envelopes`() {
        val fixture = fixtures()
        assertEquals("Ab12", ReferralContract.parsePending(fixture.getJSONObject("referralPending")).candidate.code)
        assertEquals("0123456789abcdef0123456789abcdef",
            ReferralContract.parseCleared(fixture.getJSONObject("referralCleared")).requestId)
        val parsed = ReferralContract.parseInfo(info()).referral
        assertEquals("Ab12", parsed.code)
        assertEquals(3L, parsed.trialBonusDays)
        assertEquals(10000L, parsed.discount.amountMinor)
        assertEquals("eligible", parsed.discount.state)
        assertEquals(0L, parsed.waitingDays)
        assertNull(parsed.attribution.receiptId)
    }

    @Test fun `field aliases extensions omissions and coercion are rejected`() {
        val bad = listOf(info().put("extra", true), info().put("request_id", 7), info().put("request_id", "bad"),
            info().apply { getJSONObject("referral").put("extra", 1) },
            info().apply { getJSONObject("referral").remove("terms_version") },
            info().apply { getJSONObject("referral").getJSONObject("benefits").put("trial_bonus_days", "3") },
            info().apply { getJSONObject("referral").getJSONObject("benefits").put("trial_bonus_days", 3.0) },
            info().apply { getJSONObject("referral").getJSONObject("benefits").put("trial_bonus_days", -1) },
            info().apply { getJSONObject("referral").getJSONObject("rewards").put("waiting_days", JSONObject.NULL) },
            info().apply { getJSONObject("referral").put("account_ref", "account") },
            info().apply { getJSONObject("referral").put("terms_version", "other-version") })
        bad.forEach { assertThrows(IllegalStateException::class.java) { ReferralContract.parseInfo(it) } }
    }

    @Test fun `invitation links must match the server own code and accepted hosts`() {
        listOf("https://evil.example/", "https://terlimo.xyz/?ref=uOther", "http://terlimo.xyz/?ref=uAb12").forEach { link ->
            assertThrows(IllegalStateException::class.java) {
                ReferralContract.parseInfo(info().apply { getJSONObject("referral").getJSONObject("links").put("web", link) })
            }
        }
    }

    @Test fun `attribution is authoritative display data and never a full correlation proof`() {
        val receiptId = "42345678-1234-1234-1234-123456789abc"
        val event = info().apply {
            getJSONObject("referral").getJSONObject("attribution").put("state", "attached").put("receipt_id", receiptId)
        }
        val parsed = ReferralContract.parseInfo(event)
        assertEquals(receiptId, parsed.referral.attribution.receiptId)
        // The parsed info contains no installation, registration token, or candidate ownership proof.
        assertEquals(setOf("account_ref", "code", "links", "attribution", "benefits", "rewards", "terms_version"),
            event.getJSONObject("referral").keys().asSequence().toSet())
    }

    @Test fun `inconsistent attribution and unknown discount states fail closed`() {
        val bad = listOf(
            info().apply { getJSONObject("referral").getJSONObject("attribution").put("state", "attached") },
            info().apply { getJSONObject("referral").getJSONObject("attribution").put("state", "rejected").put("reason", "self") },
            info().apply { getJSONObject("referral").getJSONObject("attribution").put("state", "unknown") },
            info().apply { getJSONObject("referral").getJSONObject("benefits").getJSONObject("discount").put("state", "free") },
            info().apply { getJSONObject("referral").getJSONObject("benefits").getJSONObject("discount").put("amount_minor", -1) })
        bad.forEach { assertThrows(IllegalStateException::class.java) { ReferralContract.parseInfo(it) } }
    }

    @Test fun `cleared and pending shapes cannot be swapped or extended`() {
        val f = fixtures()
        assertThrows(IllegalStateException::class.java) { ReferralContract.parsePending(f.getJSONObject("referralCleared")) }
        assertThrows(IllegalStateException::class.java) { ReferralContract.parseCleared(f.getJSONObject("referralPending")) }
        assertThrows(IllegalStateException::class.java) {
            ReferralContract.parsePending(fixtures().getJSONObject("referralPending").apply {
                getJSONObject("candidate").put("code", "URL/Ab12")
            })
        }
        assertThrows(IllegalStateException::class.java) {
            ReferralContract.parseCleared(fixtures().getJSONObject("referralCleared").apply {
                getJSONObject("candidate").put("id", "12345678-1234-1234-1234-123456789abc")
            })
        }
    }

    @Test fun `unknown and history pending error texts make no successful eligibility claim`() {
        assertTrue(ReferralContract.errorText("REFERRAL_HISTORY_PENDING").contains("недоступны"))
        assertTrue(ReferralContract.errorText("UNKNOWN_ERROR").contains("не подтверждён"))
        assertTrue(ReferralContract.errorText("TRANSPORT").contains("не подтверждён"))
    }

    @Test fun `registration success and optional receipt id alone are not referral success`() {
        for (name in listOf("referralRegistered", "registrationWithReceipt")) {
            assertThrows(IllegalStateException::class.java) { ReferralContract.parseInfo(fixtures().getJSONObject(name)) }
            assertThrows(IllegalStateException::class.java) { ReferralContract.parsePending(fixtures().getJSONObject(name)) }
        }
    }

    @Test fun `published keyed registration pending registered and expiry fixtures parse exact wire fields`() {
        val f = registrationFixtures()
        val pending = (ReferralContract.parseRegistration(f.getJSONObject("pending")) as ReferralRegistrationResult.Pending).pending
        assertEquals("registration-key-0001", pending.correlation.idempotencyKey)
        assertEquals("aaaaaaaa-1234-1234-1234-123456789abc", pending.correlation.registrationId)
        assertEquals("tok123", pending.token)
        assertEquals(600, pending.expiresIn)
        val receipt = (ReferralContract.parseRegistration(f.getJSONObject("registered")) as ReferralRegistrationResult.Registered).receipt
        assertEquals(pending.correlation.registrationId, receipt.registrationId)
        assertEquals("attached", receipt.state)
        val expired = f.getJSONObject("expired")
        assertEquals(pending.correlation, ReferralContract.parseRegistrationExpiry(expired.getJSONObject("body"), expired.getInt("http_status")).correlation)
    }

    @Test fun `keyed parser rejects old registered and receipt id envelopes without full correlation`() {
        listOf("referralRegistered", "registrationWithReceipt").forEach {
            assertThrows(IllegalStateException::class.java) { ReferralContract.parseRegistration(fixtures().getJSONObject(it)) }
        }
    }

    @Test fun `pending malformed correlation token link numbers and times are rejected`() {
        fun pending() = registrationFixtures().getJSONObject("pending")
        val invalid = listOf(pending().put("schema_version", "2.0"), pending().put("server_time", "bad"),
            pending().apply { getJSONObject("registration").remove("referral_registration") },
            pending().apply { getJSONObject("registration").getJSONObject("referral_registration").put("registration_id", "tok123") },
            pending().apply { getJSONObject("registration").put("deep_link", "https://t.me/other_bot?start=tok123") },
            pending().apply { getJSONObject("registration").put("expires_in", "600") },
            pending().apply { getJSONObject("registration").put("expires_in", 600.5) },
            pending().apply { getJSONObject("registration").put("expires_in", 0) },
            pending().apply { getJSONObject("registration").put("expires_at", "2026-10-99T00:00:00Z") })
        invalid.forEach { assertThrows(IllegalStateException::class.java) { ReferralContract.parseRegistration(it) } }
    }

    @Test fun `full receipts exclude invented installation token and enforce exact terminal reason`() {
        fun registered() = registrationFixtures().getJSONObject("registered")
        val bad = listOf(
            registered().apply { getJSONObject("registration").getJSONObject("referral_attribution").put("installation_id", "installation") },
            registered().apply { getJSONObject("registration").getJSONObject("referral_attribution").put("registration_token", "tok123") },
            registered().apply { getJSONObject("registration").getJSONObject("referral_attribution").remove("idempotency_key") },
            registered().apply { getJSONObject("registration").getJSONObject("referral_attribution").put("state", "pending") },
            registered().apply { getJSONObject("registration").getJSONObject("referral_attribution").put("state", "rejected") },
            registered().apply { getJSONObject("registration").put("referral_attribution_receipt_id", "aaaaaaaa-1234-1234-1234-123456789abc") })
        bad.forEach { assertThrows(IllegalStateException::class.java) { ReferralContract.parseRegistration(it) } }
        val rejected = registered().apply { getJSONObject("registration").getJSONObject("referral_attribution")
            .put("state", "rejected").put("reason", "ineligible") }
        assertEquals("rejected", (ReferralContract.parseRegistration(rejected) as ReferralRegistrationResult.Registered).receipt.state)
    }

    @Test fun `expiry requires authoritative 410 complete correlation and retryable false`() {
        fun body() = registrationFixtures().getJSONObject("expired").getJSONObject("body")
        listOf(400, 404, 409, 500).forEach { status ->
            assertThrows(IllegalStateException::class.java) { ReferralContract.parseRegistrationExpiry(body(), status) }
        }
        val bad = listOf(body().apply { remove("details") }, body().put("retryable", true), body().put("retryable", "false"),
            body().put("code", "BAD_MESSAGE"), body().apply { getJSONObject("details").put("state", "pending") },
            body().apply { getJSONObject("details").getJSONObject("referral_registration").remove("candidate_id") })
        bad.forEach { assertThrows(IllegalStateException::class.java) { ReferralContract.parseRegistrationExpiry(it, 410) } }
    }
}

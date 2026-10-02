package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class PurchaseProductAttemptsTest {
    private class Store(var raw: String? = null) : PurchaseAttemptStore {
        override fun read() = raw
        override fun write(encoded: String) { raw = encoded }
    }
    private var next = 0
    private fun attempts(store: Store) = PurchaseAttempts(store) { "terlimo-product-key-${++next}" }
    private fun selection(ids: List<String> = listOf("slot-a")) = PurchaseSelection(
        "terlimo-30d", "days:30", "card", "subscription", "target-a", "2026-11-01T00:00:00Z", ids)

    @Test fun aFullSelectionRetrySurvivesRestartAndOrderDoesNotRotateKeys() {
        val store = Store()
        val original = attempts(store).beginQuote(selection(listOf("slot-b", "slot-a")))
        assertEquals(selection(listOf("slot-a", "slot-b")), original.selection)
        val restarted = attempts(store).beginQuote(selection(listOf("slot-a", "slot-b")))
        assertEquals(original, restarted)
        assertEquals(2, next)
        assertEquals(original, PurchaseAttemptCodec.decode(store.raw))
    }

    @Test fun eachChangedBusinessSelectionRotatesBothKeys() {
        val changes = listOf(selection(emptyList()), selection().copy(targetEntitlementId = "target-b"),
            selection().copy(targetValidUntil = "2026-12-01T00:00:00Z"), selection().copy(method = "sbp"),
            selection().copy(planId = "terlimo-3m", durationCode = "months:3"),
            selection(emptyList()).copy(productKind = "device_addon", durationCode = "until:2026-11-01T00:00:00Z"))
        changes.forEach { changed ->
            val store = Store()
            val original = attempts(store).beginQuote(selection())
            val after = attempts(store).beginQuote(changed)
            assertNotEquals(original.quoteKey, after.quoteKey)
            assertNotEquals(original.paymentKey, after.paymentKey)
            assertNotEquals(original.attemptId, after.attemptId)
        }
    }

    @Test fun paymentRetryKeepsSelectionAndRebindingADifferentQuoteRotatesKeys() {
        val store = Store()
        val engine = attempts(store)
        val original = engine.beginQuote(selection())
        engine.bindQuote("quote-a")
        val payment = attempts(store).beginPayment("quote-a")
        assertEquals(original.paymentKey, payment.paymentKey)
        assertEquals(selection(), payment.selection)
        val different = engine.bindQuote("quote-b")!!
        assertNotEquals(payment.paymentKey, different.paymentKey)
        assertEquals(selection(), different.selection)
        assertEquals(different.paymentKey, attempts(store).beginPayment("quote-b").paymentKey)
    }

    @Test fun legacyStoredAttemptRemainsReadableAndKeepsItsOriginalRetryKeys() {
        val legacy = PurchaseAttempt("attempt", "p30", "days:30", "card", null,
            "terlimo-legacy-key-a", "terlimo-legacy-key-b")
        val store = Store(PurchaseAttemptCodec.encode(legacy))
        val migrated = attempts(store).beginQuote("p30", "days:30", "card")
        assertEquals(legacy.quoteKey, migrated.quoteKey)
        assertEquals(legacy.paymentKey, migrated.paymentKey)
        assertEquals(legacy.attemptId, migrated.attemptId)
        assertEquals(PurchaseSelection("p30", "days:30", "card"), migrated.selection)
    }

    @Test fun addonUntilSelectionRoundTripsAcrossRestart() {
        val store = Store()
        val selected = PurchaseSelection("terlimo-extra-device", "until:2026-11-01T00:00:00Z", "card",
            "device_addon", "target-a", "2026-11-01T00:00:00Z")
        val original = attempts(store).beginQuote(selected)
        assertEquals(original, attempts(store).beginQuote(selected))
    }
}

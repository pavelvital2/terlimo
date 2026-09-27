package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test

class PurchaseAttemptsTest {
    private class MemoryStore(var value: String? = null) : PurchaseAttemptStore {
        override fun read(): String? = value
        override fun write(encoded: String) { value = encoded }
    }

    private class KeyFactory {
        var counter = 0
        fun next(): String = "terlimo-test-key-" + (++counter).toString().padStart(4, '0')
    }

    private fun engine(store: MemoryStore, keys: KeyFactory): PurchaseAttempts {
        return PurchaseAttempts(store) { keys.next() }
    }

    @Test
    fun quoteRetryReusesTheSameKeyAcrossTimeoutAndProcessRestart() {
        val store = MemoryStore()
        val keys = KeyFactory()
        val attempts = engine(store, keys)
        val first = attempts.beginQuote("p30", "days:30", "card")
        val retry = attempts.beginQuote("p30", "days:30", "card")
        assertEquals(first.quoteKey, retry.quoteKey)
        assertEquals(first.paymentKey, retry.paymentKey)
        assertEquals(first.attemptId, retry.attemptId)
        // The retry must not have generated anything at all: the factory already produced
        // exactly the two keys of the first attempt and nothing more.
        assertEquals(2, keys.counter)

        // Simulated process restart: a brand-new policy instance over the durable store
        // must still reuse the exact persisted keys, not mint fresh ones.
        val restarted = PurchaseAttempts(store) { "brand-new-" + keys.next() }
        val afterRestart = restarted.beginQuote("p30", "days:30", "card")
        assertEquals(first.quoteKey, afterRestart.quoteKey)
        assertEquals(first.paymentKey, afterRestart.paymentKey)
        assertEquals(2, keys.counter)
    }

    @Test
    fun changedPlanOrMethodIsADeliberateNewAttemptWithNewKeys() {
        val store = MemoryStore()
        val keys = KeyFactory()
        val attempts = engine(store, keys)
        val original = attempts.beginQuote("p30", "days:30", "card")
        val otherPlan = attempts.beginQuote("p93", "months:3", "card")
        assertNotEquals(original.quoteKey, otherPlan.quoteKey)
        assertNotEquals(original.paymentKey, otherPlan.paymentKey)
        assertNotEquals(original.attemptId, otherPlan.attemptId)

        val otherMethod = attempts.beginQuote("p93", "months:3", "sbp")
        assertNotEquals(otherPlan.quoteKey, otherMethod.quoteKey)
        assertNotEquals(otherPlan.paymentKey, otherMethod.paymentKey)
    }

    @Test
    fun paymentRetryReusesTheSameKeyAndAChangedQuoteRotatesIt() {
        val store = MemoryStore()
        val keys = KeyFactory()
        val attempts = engine(store, keys)
        val quoted = attempts.beginQuote("p30", "days:30", "card")
        attempts.bindQuote("q-1")
        val payment = attempts.beginPayment("q-1")
        assertEquals(quoted.paymentKey, payment.paymentKey)

        // Ambiguous response/timeout retry of the same quote: identical key bytes.
        assertEquals(payment.paymentKey, attempts.beginPayment("q-1").paymentKey)
        PurchaseAttempts(store) { "unused-key-0001" }.beginPayment("q-1").let {
            assertEquals(payment.paymentKey, it.paymentKey)
        }

        // A different quote is a new attempt and never reuses a key bound to other data.
        val nextQuote = attempts.beginQuote("p30", "days:30", "sbp")
        attempts.bindQuote("q-2")
        val nextPayment = attempts.beginPayment("q-2")
        assertNotEquals(payment.paymentKey, nextPayment.paymentKey)
        // The new attempt's payment reuses the new attempt's own pair, never the old one.
        assertEquals(nextQuote.paymentKey, nextPayment.paymentKey)
        assertNotEquals(payment.quoteKey, nextQuote.quoteKey)
    }

    @Test
    fun restartDropsTheIdentitySoTheNextPurchaseGeneratesNewKeys() {
        val store = MemoryStore()
        val keys = KeyFactory()
        val attempts = engine(store, keys)
        val first = attempts.beginQuote("p30", "days:30", "card")
        attempts.bindQuote("q-1")
        attempts.beginPayment("q-1")
        attempts.restart()
        assertNull(attempts.current())
        val second = attempts.beginQuote("p30", "days:30", "card")
        assertNotEquals(first.quoteKey, second.quoteKey)
        assertNotEquals(first.paymentKey, second.paymentKey)
    }

    @Test
    fun codecRoundTripsAndReadsCorruptionAsNoAttempt() {
        val record = PurchaseAttempt(
            attemptId = "a-1", planId = "p30", durationCode = "days:30", method = "card",
            quoteId = "q-1", quoteKey = "terlimo-test-key-0001", paymentKey = "terlimo-test-key-0002")
        assertEquals(record, PurchaseAttemptCodec.decode(PurchaseAttemptCodec.encode(record)))
        assertNull(PurchaseAttemptCodec.decode(null))
        assertNull(PurchaseAttemptCodec.decode(""))
        assertNull(PurchaseAttemptCodec.decode("{}"))
        assertNull(PurchaseAttemptCodec.decode("not-json"))
        assertNull(PurchaseAttemptCodec.decode(
            """{"attempt_id":"a-1","plan_id":null,"duration_code":null,"method":null,
                "quote_id":null,"quote_key":"short","payment_key":"terlimo-test-key-0002"}"""))
        assertNull(PurchaseAttemptCodec.decode(
            """{"attempt_id":"a-1","plan_id":null,"duration_code":null,"method":null,
                "quote_id":null,"quote_key":"terlimo-test-key-0001"}"""))
    }

    @Test
    fun generatedKeysMatchTheNativeForwardedShape() {
        val generated = PurchaseAttempts(MemoryStore()).beginQuote("p30", "days:30", "card")
        assertTrue(PurchaseAttemptCodec.usableKey(generated.quoteKey))
        assertTrue(PurchaseAttemptCodec.usableKey(generated.paymentKey))
        assertTrue(generated.quoteKey.length in 16..128)
        assertFalse(generated.quoteKey.contains('\n'))
        assertFalse(generated.quoteKey.contains('\r'))
        assertNotEquals(generated.quoteKey, generated.paymentKey)
        assertNotNull(generated.attemptId)
    }
}

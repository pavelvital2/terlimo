package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertThrows
import org.junit.Test

class SubscriptionImportInputTest {
    @Test fun acceptsCanonicalSubscriptionDeepLink() {
        val link = "whitelists://subscription?v=1#c3ludGhldGlj"
        assertEquals(link, SubscriptionImportInput.normalize("  $link  "))
    }

    @Test fun rejectsOtherSchemeHostAndAuthorityFields() {
        listOf(
            "https://subscription?v=1#x",
            "whitelists://other?v=1#x",
            "whitelists://user@subscription?v=1#x",
            "whitelists://subscription:9?v=1#x",
            "whitelists://subscription/path?v=1#x",
            "whitelists://subscription?v=2#x",
            "whitelists://subscription?v=1",
        ).forEach { value ->
            assertThrows(IllegalArgumentException::class.java) {
                SubscriptionImportInput.normalize(value)
            }
        }
    }

    @Test fun rejectsControlCharactersAndOversize() {
        assertThrows(IllegalArgumentException::class.java) {
            SubscriptionImportInput.normalize("whitelists://subscription?v=1#x\ny")
        }
        assertThrows(IllegalArgumentException::class.java) {
            SubscriptionImportInput.normalize("whitelists://subscription?v=1#" + "x".repeat(65_537))
        }
        assertThrows(IllegalArgumentException::class.java) {
            SubscriptionImportInput.normalize(" ".repeat(65_537) + "whitelists://subscription?v=1#x")
        }
    }

    @Test fun acceptsIntentionalRepeatBecauseNativeImportOwnsIdempotency() {
        val link = "whitelists://subscription?v=1#c2FtZQ"
        assertEquals(link, SubscriptionImportInput.normalize(link))
        assertEquals(link, SubscriptionImportInput.normalize(link))
    }
}

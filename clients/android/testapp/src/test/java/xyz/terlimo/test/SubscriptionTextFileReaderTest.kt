package xyz.terlimo.test

import java.io.ByteArrayInputStream
import org.junit.Assert.assertEquals
import org.junit.Assert.assertThrows
import org.junit.Test

class SubscriptionTextFileReaderTest {
    @Test fun readsBoundedUtf8Text() {
        val value = "whitelists://subscription?v=1#c3ludGhldGlj"
        assertEquals(value, SubscriptionTextFileReader.readBounded(ByteArrayInputStream(value.toByteArray())))
    }

    @Test fun rejectsOversizeBeforeReturningAnyText() {
        assertThrows(IllegalArgumentException::class.java) {
            SubscriptionTextFileReader.readBounded(ByteArrayInputStream(ByteArray(65_537)))
        }
    }

    @Test fun rejectsMalformedUtf8FromOctetStream() {
        listOf(
            byteArrayOf(0x77, 0x68, 0x69, 0x74, 0x65, 0x6c, 0x69, 0x73, 0x74, 0x73, 0x3a, 0xc3.toByte(), 0x28),
            byteArrayOf(0xf0.toByte(), 0x28, 0x8c.toByte(), 0x28),
        ).forEach { malformed ->
            assertThrows(IllegalArgumentException::class.java) {
                SubscriptionTextFileReader.readBounded(ByteArrayInputStream(malformed))
            }
        }
    }
}

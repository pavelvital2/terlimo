package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertSame
import org.junit.Test

class ManualCaptchaPendingOwnerTest {
    @Test fun oldWindowResultAndCleanupCannotConsumeNewPending() {
        val pending = ManualCaptchaPendingOwner<String>()
        pending.replace(ManualCaptchaPendingOwner.Pending("old", "old-result"))
        val current = ManualCaptchaPendingOwner.Pending("new", "new-result")
        pending.replace(current)

        assertNull("old JS result is dropped", pending.consume("old"))
        assertNull("old finally/cancel cannot remove the new owner", pending.consume("old"))
        assertSame(current, pending.current())
        assertEquals("new-result", pending.consume("new")?.value)
    }

    @Test fun currentSuccessInvalidatesBeforeDeliveryAndCompletesExactlyOnce() {
        val pending = ManualCaptchaPendingOwner<String>()
        pending.replace(ManualCaptchaPendingOwner.Pending("current", "success"))
        val deliveries = mutableListOf<String>()
        pending.consume("current")?.let {
            assertNull("owner must be invalidated before completion", pending.current())
            deliveries += it.value
        }
        pending.consume("current")?.let { deliveries += it.value }
        assertEquals(listOf("success"), deliveries)
    }

    @Test fun disconnectInvalidatesAndOldNotificationCannotCancelNewOwner() {
        val pending = ManualCaptchaPendingOwner<String>()
        pending.replace(ManualCaptchaPendingOwner.Pending("old", "old-result"))
        assertEquals("old-result", pending.consume("old")?.value) // Disconnect
        assertNull(pending.consume("old")) // late JS result/reopen
        pending.replace(ManualCaptchaPendingOwner.Pending("new", "new-result"))
        assertNull("old notification cancel is stale", pending.consume("old"))
        assertEquals("new", pending.current()?.owner)
        assertEquals("new-result", pending.consume("new")?.value)
    }
}

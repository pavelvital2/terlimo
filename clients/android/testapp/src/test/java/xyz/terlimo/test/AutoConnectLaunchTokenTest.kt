package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** §26.2 Activity launch token: recreation-stable, dialog/no-dialog equivalent, import-safe. */
class AutoConnectLaunchTokenTest {

    @Test fun noDialogSendConsumesTheToken() {
        val t = AutoConnectLaunchToken()
        assertEquals(11L, t.start(11L))
        t.sent()
        assertFalse(t.started())
        assertEquals(12L, t.start(12L)) // a later user open may start a fresh token
    }

    @Test fun secondStartWhilePendingIsRefused() {
        val t = AutoConnectLaunchToken()
        assertEquals(11L, t.start(11L))
        assertEquals(0L, t.start(12L))
    }

    @Test fun recreationRestoresThePendingConsent() {
        val t = AutoConnectLaunchToken()
        t.start(11L)
        t.needsConsent()
        val restored = AutoConnectLaunchToken()
        restored.restore(t.currentGeneration(), true)
        assertEquals(11L, restored.currentGeneration())
        assertTrue(restored.consentReturned(11L, stillValid = true))
        assertFalse(restored.started())
    }

    @Test fun lateConsentOfAnInvalidatedTokenIsRejected() {
        val t = AutoConnectLaunchToken()
        t.start(11L)
        t.needsConsent()
        t.invalidate() // Off / import / identity change
        assertFalse(t.consentReturned(11L, stillValid = true))
    }

    @Test fun consentOfAnotherGenerationIsRejected() {
        val t = AutoConnectLaunchToken()
        t.start(11L)
        t.needsConsent()
        assertFalse(t.consentReturned(10L, stillValid = true))
    }
}

package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class NotificationTextTest {
    @Test fun namesSelectedServerBoundedAndSecretFree() {
        assertEquals("Сервер: STEP036 device node", NotificationText.serverLabel("STEP036 device node"))
        assertEquals("Сервер: X", NotificationText.serverLabel("  X  "))
        assertNull(NotificationText.serverLabel(null))
        assertNull(NotificationText.serverLabel("   "))
        assertEquals(48 + "Сервер: ".length, NotificationText.serverLabel("z".repeat(80))!!.length)
    }
}

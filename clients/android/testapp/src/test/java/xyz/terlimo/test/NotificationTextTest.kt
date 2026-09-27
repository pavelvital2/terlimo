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

    @Test fun trafficSegmentIsShownForConnectedEvenAtZeroAndHiddenOtherwise() {
        assertEquals("↓0 Б ↑0 Б", NotificationText.trafficSegment("Connected", TrafficSnapshot()))
        assertNull(NotificationText.trafficSegment("Idle", TrafficSnapshot()))
        assertNull(NotificationText.trafficSegment("CatalogReady", TrafficSnapshot(rxTotal = 10)))
        assertEquals("↓1.0 КБ ↑2.0 КБ", NotificationText.trafficSegment(
            "Connected", TrafficSnapshot(rxTotal = 1024, txTotal = 2048)))
    }
}

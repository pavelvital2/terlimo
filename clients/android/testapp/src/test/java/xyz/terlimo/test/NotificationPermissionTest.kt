package xyz.terlimo.test

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class NotificationPermissionTest {
    @Test fun requestsAtMostOnceAndOnlyOnModernSdkWithoutGrant() {
        assertTrue(NotificationPermission.shouldRequest(33, granted = false, alreadyAsked = false))
        assertTrue(NotificationPermission.shouldRequest(34, granted = false, alreadyAsked = false))
        assertFalse(NotificationPermission.shouldRequest(34, granted = false, alreadyAsked = true))
        assertFalse(NotificationPermission.shouldRequest(34, granted = true, alreadyAsked = false))
        assertFalse(NotificationPermission.shouldRequest(32, granted = false, alreadyAsked = false))
    }
}

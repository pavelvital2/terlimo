package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class LeaseExpiryPolicyTest {
    @Test fun sleepDoesNotExtendElapsedDeadline() {
        assertFalse(LeaseExpiryPolicy.due(1000, 999))
        assertTrue(LeaseExpiryPolicy.due(1000, 1000))
        assertTrue(LeaseExpiryPolicy.due(1000, 9000))
    }
    @Test fun staleSameLeaseCannotExtendButNewLeaseCanReplace() {
        assertEquals(1000L, LeaseExpiryPolicy.merge(1000, true, 2000))
        assertEquals(900L, LeaseExpiryPolicy.merge(1000, true, 900))
        assertEquals(2000L, LeaseExpiryPolicy.merge(1000, false, 2000))
    }
    @Test fun UnlimitedAndStoppedHaveNoExpiry() {
        assertFalse(LeaseExpiryPolicy.due(Long.MAX_VALUE, Long.MAX_VALUE))
        assertFalse(LeaseExpiryPolicy.due(0, 1000))
    }
}

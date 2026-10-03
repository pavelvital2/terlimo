package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertThrows
import org.junit.Test

class StorageAccessPolicyTest {
    @Test fun lockedStorageCannotExecuteReadsOrWrites() {
        var accesses = 0
        assertThrows(IllegalStateException::class.java) { withUnlockedStorage(false) { accesses++ } }
        assertEquals(0, accesses)
        withUnlockedStorage(true) { accesses++ }
        assertEquals(1, accesses)
    }
}

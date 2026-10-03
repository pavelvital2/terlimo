package xyz.terlimo.test

import java.util.Base64
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class RecoveryCodeUiTest {
    @Test
    fun completeCodeAllowsOuterTrimButNeverInternalWhitespaceOrNonAscii() {
        assertEquals("TR1.e30.c2ln", RecoveryCodeUi.normalizeCode(" \nTR1.e30.c2ln\t "))
        for (bad in listOf("", "TR1.e30", "TR1.e30.c2ln.extra", "TR1..c2ln", "TR1.e30.",
            "TR2.e30.c2ln", "TR1.e3 0.c2ln", "TR1.e30.c2\nln", "TR1.é.c2ln", "TR1.e30=.c2ln")) {
            assertNull(RecoveryCodeUi.normalizeCode(bad))
        }
    }

    @Test
    fun fullTrimmedCodeHasAnExact3500CharacterLimitWithoutTruncation() {
        val maximum = "TR1.${"a".repeat(3494)}.b"
        assertEquals(3500, maximum.length)
        assertEquals(maximum, RecoveryCodeUi.normalizeCode(" $maximum\n"))
        assertNull(RecoveryCodeUi.normalizeCode(maximum + "b"))
    }

    @Test
    fun unavailableOrActiveServiceRequiresNoRecoveryStart() {
        assertTrue(RecoveryCodeUi.canApply(usable = true, workingVpn = false))
        assertFalse(RecoveryCodeUi.canApply(usable = false, workingVpn = false))
        assertEquals("RECOVERY_UNAVAILABLE", RecoveryCodeUi.unavailableReason(false, false))
        assertEquals("RECOVERY_DISCONNECT_REQUIRED", RecoveryCodeUi.unavailableReason(true, true))
        assertEquals("RECOVERY_DISCONNECT_REQUIRED",
            RecoveryCodeUi.unavailableReason(true, false, serviceActive = true))
        assertEquals("RECOVERY_BUSY", RecoveryCodeUi.unavailableReason(true, false, busy = true))
        assertFalse(RecoveryCodeUi.canApply(true, false, serviceActive = true))
    }

    @Test
    fun optionalMalformedPublicKeyDoesNotInvalidateOrdinaryBootstrap() {
        val validKey = Base64.getUrlEncoder().withoutPadding().encodeToString(ByteArray(32))
        fun parse(key: Any): MobileBootstrapSeed? = MobileBootstrapSeed.parse(JSONObject()
            .put("base_url", "https://service.invalid").put("environment", "test")
            .put("recovery_verify_key_b64", key).toString())
        assertEquals(validKey, parse(validKey)!!.recoveryVerifyKeyB64)
        for (invalid in listOf(JSONObject.NULL, 42, "", validKey + "=", "a".repeat(42),
            validKey.dropLast(1) + "B", "not-a-key")) {
            val seed = parse(invalid)
            assertNotNull(seed)
            assertNull(seed!!.recoveryVerifyKeyB64)
            assertTrue(MobileBootstrapGate.admits("", seed))
        }
        assertNull(MobileBootstrapSeed.parse("""{"base_url":"https://service.invalid"}""")!!
            .recoveryVerifyKeyB64)
    }

    @Test
    fun fixedStatusMessagesNeverEchoAnUnrecognizedCode() {
        val privateInput = "TR1.e30.c2ln"
        assertFalse(RecoveryCodeUi.textForStatus(privateInput).contains(privateInput))
        assertTrue(RecoveryCodeUi.textForStatus("RECOVERY_CANCELLED").contains("не изменены"))
        assertTrue(RecoveryCodeUi.textForStatus("RECOVERY_PERSIST").contains("не изменены"))
        assertTrue(RecoveryCodeUi.textForStatus("RECOVERY_SUCCESS").contains("сохранено"))
        assertTrue(RecoveryCodeUi.textForStatus("RECOVERY_SIGNATURE").contains("Подпись"))
    }
}

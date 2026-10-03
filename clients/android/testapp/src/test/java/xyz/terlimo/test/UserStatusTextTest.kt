package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class UserStatusTextTest {
    @Test fun catalogTimeoutRetainsCodeWithoutClaimingPersistenceWasUnchanged() {
        val text = UserStatusText.error("CATALOG_TIMEOUT").orEmpty()
        assertTrue(text.startsWith("CATALOG_TIMEOUT\n"))
        assertFalse(text.contains("HOST_ERROR"))
        assertFalse(text.contains("не изменены"))
        assertTrue(text.contains("сохранённые данные не удаляйте"))
    }
    @Test fun phasesKeepExistingLabelsAndConnectedEvidence() {
        assertEquals("Регистрация установки", UserStatusText.phase("Registering"))
        assertEquals(
            "Подключено: WireGuard, DNS и HTTPS подтверждены",
            UserStatusText.phase("Connected")
        )
        assertEquals("Подключение остановлено", UserStatusText.phase("UnexpectedPhase"))
    }

    @Test fun killSwitchLabelDoesNotPromiseDisconnectAsTheOnlyExit() {
        val label = UserStatusText.phase("KillSwitch")
        assertTrue(label.startsWith("Сеть заблокирована"))
        assertFalse(label.contains("до явного отключения"))
        assertTrue(label.contains("выберите другой сервер"))
    }

    @Test fun pendingAndUnknownResumeTheSavedOperationWithoutReplacingIdentity() {
        for (code in listOf("OPERATION_PENDING", "OPERATION_UNKNOWN")) {
            val text = UserStatusText.error(code).orEmpty()
            assertTrue(text.contains("Повторите подключение"))
            assertTrue(text.contains("ту же операцию"))
            assertTrue(text.contains("Не удаляйте"))
            assertFalse(text.contains("импорт"))
            assertTrue(text.contains("не создавайте новый ключ"))
        }
    }

    @Test fun connectionErrorsOfferCurrentActionsInsteadOfLegacyImport() {
        for (code in listOf("HOST_ERROR", "IMPORT_REQUIRED", "DIFFERENT_SUBSCRIPTION", "CATALOG_EXPIRED",
            "CATALOG_TIMEOUT", "BAD_MESSAGE", "CATALOG_INVALID", "OPERATION_PENDING", "OPERATION_UNKNOWN",
            "LEASE_CONFLICT", "APPROVAL_REQUIRED", "LEASE_EXPIRED", "TRANSPORT_TIMEOUT", "TRANSPORT_FAILED",
            "EOF", "AUTH_REQUIRED", "BACKEND_UNAVAILABLE", "SESSION_NOT_READY", "VPN_STOPPED")) {
            val text = UserStatusText.error(code).orEmpty().lowercase()
            assertFalse("legacy import instruction: $code", text.contains("импорт"))
            assertFalse("legacy subscription action: $code", text.contains("сохранённую подписку"))
            assertFalse("legacy profile instruction: $code", text.contains("профиль"))
            assertFalse("legacy link instruction: $code", text.contains("подписанную ссылку"))
        }
    }

    @Test fun revokeMessagesDoNotSuggestBypassingServerDecision() {
        assertTrue(UserStatusText.error("DEVICE_REVOKED").orEmpty().contains("отозвано на сервере"))
        assertTrue(UserStatusText.error("GRANT_REVOKED").orEmpty().contains("отозван на сервере"))
        assertTrue(UserStatusText.error("VPN_REVOKED").orEmpty().contains("Разрешите VPN"))
    }

    @Test fun leaseAndSubscriptionExpiryHaveDifferentRecovery() {
        val lease = UserStatusText.error("LEASE_EXPIRED").orEmpty()
        val subscription = UserStatusText.error("SUBSCRIPTION_EXPIRED").orEmpty()
        assertNotEquals(lease, subscription)
        assertTrue(lease.contains("сеанс"))
        assertTrue(subscription.contains("Срок подписки"))
        assertTrue(subscription.contains("Продлите"))
    }

    @Test fun networkMessagesStayWithinKnownEvidence() {
        assertTrue(UserStatusText.error("PHYSICAL_NETWORK_UNAVAILABLE").orEmpty().contains("физической сети"))
        assertTrue(UserStatusText.error("PHYSICAL_NETWORK_LOST").orEmpty().contains("Физическая сеть"))
        val dns = UserStatusText.error("DNS_FAILED").orEmpty()
        assertTrue(dns.contains("Проверка DNS через VPN не пройдена"))
        assertFalse(dns.contains("сервер DNS"))
        assertFalse(dns.contains("WireGuard не"))
    }

    @Test fun terminalTrustAndProofFailuresAreNotPresentedAsBypassable() {
        assertTrue(UserStatusText.error("TRUST_FAILED").orEmpty().contains("Не обходите проверку"))
        assertTrue(UserStatusText.error("PROOF_INVALID").orEmpty().contains("Не обходите проверку"))
    }

    @Test fun cleanupAndVpnPermissionGiveActionableRecovery() {
        assertTrue(UserStatusText.error("CLEANUP_FAILED").orEmpty().contains("отключить VPN"))
        assertTrue(UserStatusText.error("VPN_PERMISSION_DENIED").orEmpty().contains("системный запрос"))
    }

    @Test fun unknownInputUsesGenericHostErrorWithoutLeakingInput() {
        val secret = "server said token=super-secret"
        val generic = UserStatusText.error("HOST_ERROR")
        val unknown = UserStatusText.error(secret)
        assertEquals(generic, unknown)
        assertFalse(unknown.orEmpty().contains(secret))
        assertFalse(unknown.orEmpty().contains("super-secret"))
        assertNull(UserStatusText.error(null))
    }

    @Test fun realNativeCodesStayIdentifiableWithoutEchoingUnknownUppercaseText() {
        for (code in listOf("DIFFERENT_SUBSCRIPTION", "SAVED_STATE_INVALID", "CATALOG_EXPIRED", "BAD_WG_CONFIG", "LOCAL_RELAY_FAILED", "VPN_SETUP_FAILED", "RETRY_EXHAUSTED", "TURN_CAPACITY"))
            assertTrue(UserStatusText.error(code).orEmpty().startsWith("$code\n"))
        assertEquals(UserStatusText.error("HOST_ERROR"), UserStatusText.error("PRIVATE_SECRET_STRING"))
    }
}

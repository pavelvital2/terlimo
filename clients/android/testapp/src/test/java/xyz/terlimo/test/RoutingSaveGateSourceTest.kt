package xyz.terlimo.test

import java.nio.file.Files
import java.nio.file.Paths
import org.junit.Assert.*
import org.junit.Test

class RoutingSaveGateSourceTest {
    private fun source(path: String) = String(Files.readAllBytes(Paths.get(path)), Charsets.UTF_8)

    @Test fun editorUsesOneGateForButtonAndSave() {
        val activity = source("src/main/java/xyz/terlimo/test/RoutingSettingsActivity.kt")
        assertEquals(2, Regex("RoutingEditGate\\.allowsSave\\(SessionService\\.routingEditSnapshot\\(\\)\\)").findAll(activity).count())
        assertFalse(activity.contains("phase in setOf(\"Idle\", \"Error\")"))
        assertFalse(activity.contains("phase !in setOf(\"Idle\", \"Error\")"))
    }

    @Test fun snapshotCombinesPhaseWithAuthoritativeTunnelState() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(service.contains("internal fun routingEditSnapshot()"))
        assertTrue(service.contains("RoutingEditState.tunnelOf("))
        // Authoritative markers of an actually applied/applying/held tunnel.
        assertTrue(service.contains("activeVpnConfig = service.activeVpnConfig != null"))
        assertTrue(service.contains("sleepPaused = service.sleepPaused"))
        assertTrue(service.contains("networkRecovery = service.networkRecovery != null"))
        assertTrue(service.contains("nativeStopped = service.recoveryNativeStopped"))
        assertTrue(service.contains("applying = service.vpnApplying.get()"))
    }

    @Test fun teardownReleasesNativeStoppedMarkerOnSuccessfulDownBeforePublish() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        val teardown = service.substringAfter("val clean = cleanCurrent && cleanOld")
        val releaseAt = teardown.indexOf("if (clean) recoveryNativeStopped = true")
        val publishAt = teardown.indexOf("publish(SessionRetention.onStop(")
        assertTrue(teardown.contains("activeVpnConfig = null"))
        assertTrue(releaseAt in 1 until publishAt)
    }

    @Test fun emptyIncludeKeepsPreviousPolicyWithClearMessage() {
        val activity = source("src/main/java/xyz/terlimo/test/RoutingSettingsActivity.kt")
        assertTrue(activity.contains("RoutingSettingsApply.shouldPersist(mode, checked)"))
        assertTrue(activity.contains("Выберите хотя бы одно приложение"))
        assertTrue(activity.contains("compareAndSetRoutingSettings"))
    }

    @Test fun saveNeverAppliesToActiveTunnel() {
        val activity = source("src/main/java/xyz/terlimo/test/RoutingSettingsActivity.kt")
        assertFalse(activity.contains("setRoutingPlan"))
        assertFalse(activity.contains("Tunnel.State"))
        assertFalse(activity.contains("setState("))
    }

    @Test fun nextConnectReadsWholePersistedSnapshot() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(service.contains("storage.readRoutingSettings(protectedPackages, presets)"))
    }

    @Test fun storeOwnerKeepsRoutingAcrossNativePersistWithRevisionFence() {
        val store = source("src/main/java/xyz/terlimo/test/InstallationStore.kt")
        assertTrue(store.contains("withSubscriptionState(readLocked(), link, stateB64)"))
        assertTrue(store.contains("if (existing?.revision != expectedRevision) return@synchronized false"))
        assertTrue(store.contains("private val LOCK = Any()"))
    }
}

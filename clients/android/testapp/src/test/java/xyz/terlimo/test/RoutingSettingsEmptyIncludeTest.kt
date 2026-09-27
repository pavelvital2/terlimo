package xyz.terlimo.test

import java.nio.file.Files
import java.nio.file.Paths
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class RoutingSettingsEmptyIncludeTest {
    private fun source(path: String) =
        String(Files.readAllBytes(Paths.get(path)), Charsets.UTF_8)

    @Test fun emptyIncludeIsNotPersistedOtherModesAre() {
        assertFalse(RoutingSettingsApply.shouldPersist(AppRoutingMode.INCLUDE_ONLY, emptySet()))
        assertTrue(RoutingSettingsApply.shouldPersist(AppRoutingMode.INCLUDE_ONLY, setOf("com.example.app")))
        assertTrue(RoutingSettingsApply.shouldPersist(AppRoutingMode.EXCLUDE, emptySet()))
        assertTrue(RoutingSettingsApply.shouldPersist(AppRoutingMode.DISABLED, emptySet()))
    }

    @Test fun editorRejectsEmptyIncludeBeforeCasWrite() {
        val src = source("src/main/java/xyz/terlimo/test/RoutingSettingsActivity.kt")
        assertTrue(src.contains("RoutingSettingsApply.shouldPersist(mode, checked)"))
        assertTrue(src.contains("Выберите хотя бы одно приложение"))
        // The guard returns before compareAndSet, so the previous revision stays persisted.
        val guard = src.indexOf("RoutingSettingsApply.shouldPersist(mode, checked)")
        val cas = src.indexOf("compareAndSetRoutingSettings")
        assertTrue(guard in 1 until cas)
    }

    @Test fun rejectedApplyKeepsPreviousPolicyAndRevision() {
        val loaded = RoutingSettingsDocument(
            revision = 4,
            routing = RoutingPolicy(AppRoutingPolicy(AppRoutingMode.EXCLUDE, setOf("com.example.app"))),
        )
        assertFalse(RoutingSettingsApply.shouldPersist(AppRoutingMode.INCLUDE_ONLY, emptySet()))
        assertEquals(4L, loaded.revision)
        assertEquals(AppRoutingMode.EXCLUDE, loaded.routing.apps.mode)
    }
}

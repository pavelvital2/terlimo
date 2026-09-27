package xyz.terlimo.test

import android.content.ContextWrapper
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.io.File
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class RoutingSettingsEncryptedStoreTest {
    @Test fun validatedCasRoundTripRetainsLastGoodOnRevisionConflict() {
        // Run on the target app process (real AndroidKeyStore) but redirect the private
        // state directory to an isolated cache dir, so the destructive fixture never
        // touches the owner's private-state.aes. The instrumentation package context is
        // not usable here: its no_backup dir is not writable by the target process.
        val base = InstrumentationRegistry.getInstrumentation().targetContext
        val sandbox = File(base.cacheDir, "routing-settings-test").apply {
            deleteRecursively(); mkdirs()
        }
        val context = object : ContextWrapper(base) {
            override fun getNoBackupFilesDir(): File = sandbox
        }
        val store = InstallationStore(context)
        val protected = setOf(context.packageName)
        val presets = QuickExclusionCatalog(emptyList())
        val first = RoutingSettingsDocument(
            revision = 1,
            routing = RoutingPolicy(RoutingPolicyNormalizer.apps(
                AppRoutingMode.DISABLED, emptyList(), emptyList(), protected, presets)),
        )
        assertTrue(store.compareAndSetRoutingSettings(null, first, protected, presets))
        assertEquals(first, store.readRoutingSettings(protected, presets))
        assertFalse(store.compareAndSetRoutingSettings(null, first.copy(revision = 2), protected, presets))
        assertEquals(1L, store.readRoutingSettings(protected, presets)?.revision)
    }
}

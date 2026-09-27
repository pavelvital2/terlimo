package xyz.terlimo.test

import android.os.Bundle
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.junit.Test
import org.junit.runner.RunWith
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.FileInputStream
import java.security.KeyStore

@RunWith(AndroidJUnit4::class)
class ExistingIdentityReadOnlyTest {
    private class AndroidProtectedFiles(private val directory: File) : ExistingIdentityReadOnlyGate.ProtectedFiles {
        override fun read(
            file: ExistingIdentityReadOnlyGate.ProtectedFile,
            maximumBytes: Int,
        ): ExistingIdentityReadOnlyGate.ReadResult {
            val target = File(directory, file.fileName)
            if (!target.exists()) {
                return ExistingIdentityReadOnlyGate.ReadResult.state(ExistingIdentityReadOnlyGate.ReadState.MISSING)
            }
            if (!target.isFile) {
                return ExistingIdentityReadOnlyGate.ReadResult.state(ExistingIdentityReadOnlyGate.ReadState.NOT_REGULAR)
            }
            return try {
                FileInputStream(target).use { input ->
                    val output = ByteArrayOutputStream(minOf(maximumBytes, 8_192))
                    val buffer = ByteArray(8_192)
                    var remaining = maximumBytes + 1
                    while (remaining > 0) {
                        val count = input.read(buffer, 0, minOf(buffer.size, remaining))
                        if (count < 0) break
                        if (count == 0) continue
                        output.write(buffer, 0, count)
                        remaining -= count
                    }
                    val bytes = output.toByteArray()
                    if (bytes.size > maximumBytes) {
                        bytes.fill(0)
                        ExistingIdentityReadOnlyGate.ReadResult.state(ExistingIdentityReadOnlyGate.ReadState.TOO_LARGE)
                    } else {
                        ExistingIdentityReadOnlyGate.ReadResult.present(bytes).also { bytes.fill(0) }
                    }
                }
            } catch (_: Throwable) {
                ExistingIdentityReadOnlyGate.ReadResult.state(ExistingIdentityReadOnlyGate.ReadState.UNREADABLE)
            }
        }
    }

    @Test
    fun matchesProtectedBaseline() {
        val instrumentation = InstrumentationRegistry.getInstrumentation()
        val files = AndroidProtectedFiles(File(instrumentation.targetContext.applicationInfo.dataDir, "no_backup"))
        val result = ExistingIdentityReadOnlyGate.evaluate(files) {
            val store = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
            object : ExistingIdentityReadOnlyGate.KeyAccess {
                override fun containsSigningAlias(): Boolean =
                    store.containsAlias(ExistingIdentityReadOnlyGate.SIGNING_ALIAS)

                override fun certificatePublicSpki(): ByteArray? =
                    store.getCertificate(ExistingIdentityReadOnlyGate.SIGNING_ALIAS)?.publicKey?.encoded
            }
        }

        instrumentation.sendStatus(2, Bundle().apply {
            putBoolean("key_present", result.keyPresent)
            putBoolean("baseline_valid", result.baselineValid)
            putBoolean("same_key", result.sameKey)
            putBoolean("files_unchanged", result.filesUnchanged)
            putBoolean("checked", result.checked)
        })
        check(result.passed()) { result.failure.name }
    }
}

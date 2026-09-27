package xyz.terlimo.test

import java.io.IOException
import java.io.InputStream

/** Drain only; process shutdown may close this pipe concurrently. Never log its contents. */
internal fun drainNativeDiagnostics(input: InputStream) {
    try {
        input.use {
            val buffer = ByteArray(4096)
            while (it.read(buffer) >= 0) { /* discard */ }
        }
    } catch (_: IOException) {
        // stdout/bridge owns process failure reporting; stderr close must not crash the host.
    }
}

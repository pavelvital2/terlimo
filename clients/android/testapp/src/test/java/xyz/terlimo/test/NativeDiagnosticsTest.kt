package xyz.terlimo.test

import java.io.ByteArrayInputStream
import java.io.InputStream
import java.io.InterruptedIOException
import org.junit.Assert.assertTrue
import org.junit.Test

class NativeDiagnosticsTest {
    @Test fun concurrentPipeCloseDoesNotEscapeDrainThread() {
        var closed = false
        drainNativeDiagnostics(object : InputStream() {
            override fun read(): Int = throw InterruptedIOException("synthetic pipe close")
            override fun close() { closed = true }
        })
        assertTrue(closed)
    }
    @Test fun eofClosesDiscardedStream() {
        var closed = false
        drainNativeDiagnostics(object : ByteArrayInputStream(ByteArray(8193)) {
            override fun close() { closed = true; super.close() }
        })
        assertTrue(closed)
    }
}

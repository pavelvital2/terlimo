package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test

class ServiceDialStderrTest {
    private fun line(stage: String, result: String, call: String = "1", candidate: String = "1", transport: String = "UDP", extra: String = "") =
        "dialstage: call=$call candidate=$candidate transport=$transport stage=$stage result=$result elapsed_ms=10$extra"

    @Test fun exactStagesAndFinishPass() {
        val begin = listOf("CANDIDATE_BEGIN", "SOCKET_BEGIN", "TLS_BEGIN", "CLIENT_BEGIN", "ALLOCATE_BEGIN", "CERT_BEGIN", "SEMAPHORE_WAIT", "DTLS_BEGIN", "FIRST_WRITE_BEGIN")
        val end = listOf("CANDIDATE_END", "SOCKET_END", "TLS_END", "CLIENT_END", "ALLOCATE_END", "CERT_END", "SEMAPHORE_ACQUIRED", "SEMAPHORE_END", "DTLS_END", "FIRST_WRITE_END")
        begin.forEach { assertNotNull(NativeStderrCodes.match(line(it, "BEGIN"))) }
        end.forEach { assertNotNull(NativeStderrCodes.match(line(it, "OK"))) }
        val finish = NativeStderrCodes.match(line("FINISH", "TIMEOUT", candidate = "0", transport = "NONE", extra = " truncated=1"))
        assertNotNull(finish)
        assertFalse(finish!!.terminal)
        assertTrue(finish.value.endsWith(":truncated=1"))
        assertNotNull(NativeStderrCodes.match(line("SOCKET_END", "OTHER", call = "18446744073709551615", candidate = "9223372036854775807")))
    }
    @Test fun rawAndMalformedInputRejected() {
        val valid = line("SOCKET_END", "OK")
        listOf(valid + " host=secret", valid.replace("UDP", "private-url"), valid.replace("stage=SOCKET_END", "stage=UNKNOWN"),
            valid.replace("result=OK", "result=raw_error"), valid.replace("call=1", "call=0"),
            line("SOCKET_BEGIN", "OK"), line("SOCKET_END", "BEGIN"), line("SOCKET_END", "OK", extra=" truncated=0"),
            line("FINISH", "OK", candidate="0", transport="NONE"), line("FINISH", "OK", extra=" truncated=0"),
            line("SEMAPHORE_ACQUIRED", "CANCELED"), line("SOCKET_END", "OK", candidate="0"),
            "dialstage: " + "X".repeat(300)).forEach { assertNull(it, NativeStderrCodes.match(it)) }
    }
    @Test fun completeNativeCapFitsIndependentBudget() {
        val mirror = NativeStderrMirror()
        repeat(64) { assertNotNull(mirror.accept(line("SOCKET_END", "OK", candidate="${it + 1}"), 0)) }
        assertNotNull(mirror.accept(line("FINISH", "OK", candidate="0", transport="NONE", extra=" truncated=1"), 0))
        repeat(11) { assertNotNull(mirror.accept(line("SOCKET_BEGIN", "BEGIN", call="2"), 0)) }
        assertNull(mirror.accept(line("SOCKET_BEGIN", "BEGIN", call="2"), 0))
        // Existing service and trace budgets were neither raised nor consumed.
        assertNotNull(mirror.accept("svcstage: ESTABLISH_DIAL_READY", 0))
        assertNotNull(mirror.accept("svctrace: gen=1 class=AUTH event=READ_BEGIN reused=0 port=0 xid=1 elapsed_ms=100", 0))
        assertNotNull(mirror.accept(line("SOCKET_BEGIN", "BEGIN", call="2"), 10_000_000_000L))
    }
}

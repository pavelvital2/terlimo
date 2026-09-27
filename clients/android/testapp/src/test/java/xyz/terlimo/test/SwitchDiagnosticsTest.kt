package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/** Fixed format and token allowlist: no raw values can ever reach the switch diagnostic line. */
class SwitchDiagnosticsTest {
    @Test fun lineUsesFixedTokensOnly() {
        assertEquals("switch stage=service_received reason=-", SwitchDiagnostics.line("service_received"))
        assertEquals("switch stage=service_reject reason=no_revision",
            SwitchDiagnostics.line("service_reject", "no_revision"))
        assertEquals("switch stage=host_send reason=ok", SwitchDiagnostics.line("host_send", "ok"))
        // A raw value that somehow reaches the formatter collapses to the fixed unknown token.
        assertEquals("switch stage=unknown reason=unknown",
            SwitchDiagnostics.line("node-1234 payload", "node-1234 payload"))
    }

    @Test fun admissionReasonMappingIsFixed() {
        assertNull(SwitchDiagnostics.admissionReason(SwitchAdmission.ALLOWED))
        assertEquals("not_connected", SwitchDiagnostics.admissionReason(SwitchAdmission.NOT_CONNECTED))
        assertEquals("pending", SwitchDiagnostics.admissionReason(SwitchAdmission.PENDING))
        assertEquals("no_revision", SwitchDiagnostics.admissionReason(SwitchAdmission.NO_REVISION))
        assertEquals("same_target", SwitchDiagnostics.admissionReason(SwitchAdmission.SAME_TARGET))
        assertEquals("unknown_target", SwitchDiagnostics.admissionReason(SwitchAdmission.UNKNOWN_TARGET))
    }

    @Test fun nativeStagesAreFixedTokens() {
        for (stage in listOf("bridge_accepted", "bridge_full", "runner_consumed",
                "runner_result_ok", "runner_result_failed")) {
            assertEquals("switch stage=native reason=$stage", SwitchDiagnostics.line("native", stage))
        }
    }
}

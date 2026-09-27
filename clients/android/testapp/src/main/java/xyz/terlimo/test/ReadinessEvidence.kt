package xyz.terlimo.test

import java.io.IOException
import java.net.SocketTimeoutException
import java.net.UnknownHostException
import javax.net.ssl.SSLException

/** Facts from one readiness observation; never holds addresses, responses or exception messages. */
internal class ReadinessEvidence {
    enum class Stage { APPLY, VPN_NETWORK, DNS, HTTPS, HTTP_STATUS, EXPECTED_EXIT, WG_STATS, READY }
    enum class ExceptionClass { NONE, TIMEOUT, DNS, TLS, IO, SECURITY, OTHER }

    var stage = Stage.APPLY
    var rx = 0L
    var tx = 0L
    var handshakePresent = false
    var handshakeFresh = false
    var vpnPresent = false
    var dnsOk = false
    /** A response was obtained over HTTPS; HTTP success is checked separately. */
    var httpsOk = false
    var httpStatusOk = false
    /** Caller compares the observed exit IP with the expected IP, without retaining either here. */
    var expectedExitOk = false
    var statsOk = false
    var exceptionClass = ExceptionClass.NONE

    val ready: Boolean
        get() = vpnPresent && dnsOk && httpsOk && httpStatusOk && expectedExitOk &&
            statsOk && handshakePresent && handshakeFresh && rx > 0 && tx > 0

    /** The earliest failed fact wins even when WG sampling was the most recent operation. */
    fun failureCode(): String = when {
        ready -> "READY"
        stage == Stage.APPLY -> "VPN_APPLY_FAILED"
        !vpnPresent -> "VPN_NETWORK_UNAVAILABLE"
        !dnsOk -> "DNS_FAILED"
        !httpsOk -> "HTTPS_FAILED"
        !httpStatusOk -> "HTTP_STATUS_FAILED"
        !expectedExitOk -> "EXPECTED_EXIT_MISMATCH"
        !statsOk -> "WG_STATS_FAILED"
        !handshakePresent -> "WG_NO_HANDSHAKE"
        !handshakeFresh -> "WG_STALE_HANDSHAKE"
        else -> "WG_NO_TRAFFIC"
    }

    fun recordException(error: Throwable) {
        exceptionClass = when (error) {
            is SocketTimeoutException -> ExceptionClass.TIMEOUT
            is UnknownHostException -> ExceptionClass.DNS
            is SSLException -> ExceptionClass.TLS
            is IOException -> ExceptionClass.IO
            is SecurityException -> ExceptionClass.SECURITY
            else -> ExceptionClass.OTHER
        }
    }

    /** Deadline is diagnostic only: it cannot turn incomplete evidence into readiness. */
    fun snapshot(deadline: Boolean = false): Map<String, Any> = linkedMapOf(
        "stage" to stage.name,
        "rx" to rx,
        "tx" to tx,
        "handshakePresent" to handshakePresent,
        "handshakeFresh" to handshakeFresh,
        "vpnPresent" to vpnPresent,
        "dnsOk" to dnsOk,
        "httpsOk" to httpsOk,
        "httpStatusOk" to httpStatusOk,
        "expectedExitOk" to expectedExitOk,
        "statsOk" to statsOk,
        "exceptionClass" to exceptionClass.name,
        "ready" to ready,
        "failureCode" to failureCode(),
        "deadline" to deadline,
    )
}

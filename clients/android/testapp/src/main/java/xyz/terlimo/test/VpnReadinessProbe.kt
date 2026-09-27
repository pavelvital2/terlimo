package xyz.terlimo.test

import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.os.Build
import android.os.Process
import android.os.SystemClock
import java.net.InetAddress
import java.net.URL
import java.net.SocketTimeoutException
import java.util.concurrent.Future
import java.util.concurrent.RejectedExecutionException
import java.util.concurrent.atomic.AtomicReference
import javax.net.ssl.HttpsURLConnection

/** No configuration, URL, DNS answer, HTTP body or peer key enters the evidence. */
internal class VpnReadinessProbe(
    private val cm: ConnectivityManager,
    private val addresses: Set<InetAddress>,
    private val dns: Set<InetAddress>,
    private val url: URL,
    private val expectedExit: String,
    private val active: () -> Boolean,
    private val sample: (ReadinessEvidence) -> Unit
) : AutoCloseable {
    private data class Progress(
        val stage: ReadinessEvidence.Stage = ReadinessEvidence.Stage.DNS,
        val dns: Boolean = false, val https: Boolean = false, val status: Boolean = false,
        val exit: Boolean = false,
        val exception: ReadinessEvidence.ExceptionClass = ReadinessEvidence.ExceptionClass.NONE
    )
    private val progress = AtomicReference(Progress())
    @Volatile private var closing = false
    @Volatile private var connection: HttpsURLConnection? = null
    @Volatile private var future: Future<*>? = null
    var elapsedMs = 0L; private set
    var expired = false; private set
    private fun live() = !closing && active()

    fun run(deadline: Long): ReadinessEvidence {
        val started = SystemClock.elapsedRealtime()
        val evidence = ReadinessEvidence().apply { stage = ReadinessEvidence.Stage.VPN_NETWORK }
        var selected: Network? = null
        var lastStart = 0L
        try {
            while (live() && SystemClock.elapsedRealtime() < deadline) {
                val vpn = cm.allNetworks.firstOrNull { network ->
                    val capabilities = cm.getNetworkCapabilities(network)
                    val properties = cm.getLinkProperties(network)
                    capabilities?.hasTransport(NetworkCapabilities.TRANSPORT_VPN) == true &&
                        (Build.VERSION.SDK_INT < 30 || capabilities.ownerUid == Process.myUid()) &&
                        properties != null && properties.linkAddresses.map { it.address }.toSet().containsAll(addresses) &&
                        properties.dnsServers.toSet() == dns
                }
                evidence.vpnPresent = vpn != null
                // Sample WG independently of DNS/HTTP success; no peer keys leave this callback.
                try { sample(evidence) } catch (error: Exception) {
                    evidence.statsOk = false; evidence.recordException(error)
                }
                if (vpn != null) {
                    if (selected != null && selected != vpn) break // Never combine evidence from two VPNs.
                    selected = vpn
                    val now = SystemClock.elapsedRealtime()
                    val previous = future
                    if ((previous == null || previous.isDone) && !progress.get().exit && now - lastStart >= 250) {
                        lastStart = now
                        try { future = slot.submit { probe(vpn, deadline) } }
                        catch (_: RejectedExecutionException) { /* one older DNS call may still be retiring */ }
                    }
                    val p = progress.get()
                    evidence.stage = p.stage
                    evidence.dnsOk = p.dns; evidence.httpsOk = p.https
                    evidence.httpStatusOk = p.status; evidence.expectedExitOk = p.exit
                    if (p.exception != ReadinessEvidence.ExceptionClass.NONE) evidence.exceptionClass = p.exception
                    if (evidence.ready) { evidence.stage = ReadinessEvidence.Stage.READY; break }
                }
                Thread.sleep(minOf(100L, (deadline - SystemClock.elapsedRealtime()).coerceAtLeast(1)))
            }
        } finally {
            elapsedMs = SystemClock.elapsedRealtime() - started
            expired = SystemClock.elapsedRealtime() >= deadline
            close()
        }
        return evidence
    }

    private fun probe(vpn: Network, deadline: Long) {
        var p = Progress()
        val budget = ProbeBudget(deadline) { SystemClock.elapsedRealtime() }
        fun publish() { if (live()) progress.set(p) }
        fun remaining(): Int {
            if (!live()) throw SocketTimeoutException()
            return budget.timeoutMs()
        }
        try {
            publish(); remaining()
            check(vpn.getAllByName(url.host).isNotEmpty())
            remaining(); p = p.copy(stage = ReadinessEvidence.Stage.HTTPS, dns = true); publish()
            val conn = vpn.openConnection(url) as HttpsURLConnection
            connection = conn
            try {
                conn.connectTimeout = remaining(); conn.readTimeout = remaining()
                conn.instanceFollowRedirects = false
                val status = conn.responseCode
                remaining(); p = p.copy(stage = ReadinessEvidence.Stage.HTTP_STATUS, https = true, status = status == 200); publish()
                if (status != 200) return
                p = p.copy(stage = ReadinessEvidence.Stage.EXPECTED_EXIT); publish()
                val body = conn.inputStream.use { input ->
                    val bytes = java.io.ByteArrayOutputStream()
                    while (bytes.size() <= 256) {
                        conn.readTimeout = remaining()
                        val next = input.read()
                        if (next < 0) break
                        bytes.write(next)
                    }
                    check(bytes.size() <= 256)
                    bytes.toString("UTF-8").trim()
                }
                remaining(); p = p.copy(exit = body == expectedExit); publish()
            } finally { conn.disconnect(); connection = null }
        } catch (error: Exception) {
            val safe = ReadinessEvidence().apply { recordException(error) }.exceptionClass
            p = p.copy(exception = safe); publish()
        }
    }

    override fun close() {
        closing = true
        future?.cancel(true)
        runCatching { connection?.disconnect() }
    }
    companion object { private val slot = ProbeSlot() }
}

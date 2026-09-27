package xyz.terlimo.test

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.os.Process
import android.os.SystemClock
import org.json.JSONArray
import org.json.JSONObject
import java.net.URL
import java.security.MessageDigest
import java.time.Instant
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicReference
import javax.net.ssl.HttpsURLConnection

/** TEST APK only: observe the real client; never renew, write state, or reconnect. */
internal class StabilityRun(private val context: Context, endUtcMillis: Long,
    startElapsed: Long, startUtcMillis: Long) : AutoCloseable {
    val budget = StabilityBudget(startElapsed, startElapsed + (endUtcMillis - startUtcMillis))
    val result = JSONObject().put("passed", false).put("reason", "SETUP_NOT_CONNECTED")
        .put("test_start_utc", Instant.ofEpochMilli(startUtcMillis).toString())
        .put("window_end_utc", Instant.ofEpochMilli(endUtcMillis).toString())
    private val samples = JSONArray().also { result.put("samples", it) }
    private val cm = context.getSystemService(ConnectivityManager::class.java)
    private val slot = ProbeSlot()
    private val connection = AtomicReference<HttpsURLConnection?>()
    private val store = InstallationStore(context)
    private val startClock = startElapsed
    private val startWall = startUtcMillis
    private var probeFailures = 0
    private var metadataFailures = 0

    private data class Snapshot(val installation: String, val registration: String, val grant: String,
        val generation: String, val seq: String, val revision: String, val expiry: Instant,
        val refreshAfter: Instant, val catalogExpiry: Instant, val slotsUsed: Int, val pending: Boolean)
    private fun snapshot(): Snapshot {
        check(java.io.File(context.noBackupFilesDir, "installation.marker").isFile) { "EXISTING_KEY_REQUIRED" }
        val state = store.read()
        val native = JSONObject(String(java.util.Base64.getUrlDecoder().decode(state.getString("state_b64")), Charsets.UTF_8))
        val catalog = native.getJSONObject("catalog")
        val access = catalog.getJSONArray("nodes").getJSONObject(0).getJSONObject("access")
        fun decimal(o: JSONObject, k: String) = o.getString(k).also { check(it.matches(Regex("[0-9]{1,32}"))) }
        return Snapshot(native.getString("installation_id"), catalog.getString("registration_id"), access.getString("grant_id"),
            decimal(access, "generation"), decimal(access, "lease_seq"), decimal(catalog, "revision"),
            Instant.parse(access.getString("expires_at")), Instant.parse(catalog.getString("refresh_after")),
            Instant.parse(catalog.getString("catalog_expires_at")), catalog.getInt("slots_used"),
            native.has("pending") && !native.isNull("pending"))
    }
    private fun ownVpn(): Network? = cm.allNetworks.firstOrNull {
        val cap = cm.getNetworkCapabilities(it)
        cap?.hasTransport(NetworkCapabilities.TRANSPORT_VPN) == true && cap.ownerUid == Process.myUid()
    }
    private fun probe(network: Network, url: URL, expected: String): JSONObject {
        val out = JSONObject().put("dns", false).put("https", false).put("status_ok", false).put("expected_exit", false)
        val until = minOf(SystemClock.elapsedRealtime() + 7500, budget.operationDeadline())
        val future = try { slot.submit {
            fun remaining(): Int {
                check(!Thread.currentThread().isInterrupted)
                return (until - SystemClock.elapsedRealtime()).coerceAtMost(3000).toInt().also { check(it > 0) }
            }
            check(network.getAllByName(url.host).isNotEmpty()); remaining(); out.put("dns", true)
            val conn = network.openConnection(url) as HttpsURLConnection
            connection.set(conn)
            try {
                conn.connectTimeout = remaining(); conn.readTimeout = remaining(); conn.instanceFollowRedirects = false
                val status = conn.responseCode; remaining()
                out.put("https", true).put("status_ok", status == 200)
                if (status == 200) {
                    val bytes = conn.inputStream.use { input ->
                        val buffer = java.io.ByteArrayOutputStream()
                        while (buffer.size() <= 256) {
                            conn.readTimeout = remaining()
                            val b = input.read(); if (b < 0) break; buffer.write(b)
                        }
                        check(buffer.size() <= 256); buffer.toString("UTF-8").trim()
                    }
                    remaining(); out.put("expected_exit", bytes == expected)
                }
            } finally { connection.compareAndSet(conn, null); conn.disconnect() }
        } } catch (_: java.util.concurrent.RejectedExecutionException) {
            return JSONObject().put("ok", false).put("failure", "PROBE_BUSY")
        }
        return try {
            future.get((until - SystemClock.elapsedRealtime()).coerceAtLeast(1), TimeUnit.MILLISECONDS)
            out.put("ok", out.getBoolean("dns") && out.getBoolean("https") && out.getBoolean("status_ok") && out.getBoolean("expected_exit"))
        } catch (e: Exception) {
            // Never read the worker's mutable JSON after timeout, nor emit exception messages.
            future.cancel(true); connection.getAndSet(null)?.disconnect()
            JSONObject().put("ok", false).put("failure", if (e is java.util.concurrent.TimeoutException) "PROBE_TIMEOUT" else "PROBE_FAILED")
        }
    }

    fun run(observe: () -> ViewState, checkpoint: () -> Unit) {
        val connected = SystemClock.elapsedRealtime()
        result.put("connected_utc", Instant.now().toString())
        if (!budget.connected(connected)) { result.put("reason", "LATE_CONNECTED_NO_FULL_HOLD"); return }
        val original = snapshot()
        check(original.expiry > Instant.now() && original.expiry <= Instant.now().plusSeconds(901)) { "INITIAL_LEASE_NOT_FRESH" }
        val key = MessageDigest.getInstance("SHA-256").digest(store.publicSpki())
        val network = ownVpn() ?: run { result.put("reason", "OWN_VPN_MISSING"); return }
        val settings = JSONObject(context.assets.open("test-probe.json").bufferedReader().use { it.readText() })
        val url = URL(settings.getString("probe_url")); check(url.protocol == "https" && url.userInfo == null)
        val expected = settings.getString("expected_exit_ip")
        result.put("original_expiry_utc", original.expiry.toString()).put("original_seq", original.seq)
            .put("original_refresh_after_utc", original.refreshAfter.toString()).put("running", true)
            .put("reason", "RUNNING")
        checkpoint()
        var nextProbe = connected
        var previous = original
        var renewed = false
        var afterOriginalExpiry = false
        var identityOk = true
        try {
            while (true) {
                val now = SystemClock.elapsedRealtime()
                val state = observe()
                if (state.phase != "Connected") { result.put("reason", "CLIENT_LEFT_CONNECTED"); break }
                if (now >= budget.operationDeadline()) { result.put("reason", "OVERALL_DEADLINE"); break }
                if (now >= nextProbe || now >= budget.holdEnd()) {
                    val row = JSONObject().put("start_utc", Instant.now().toString()).put("elapsed_ms", now - connected)
                        .put("phase", state.phase).put("same_own_vpn", ownVpn() == network)
                    val clockOk = kotlin.math.abs(System.currentTimeMillis() - (startWall + now - startClock)) <= 2000
                    row.put("clock_consistent", clockOk)
                    val p = if (row.getBoolean("same_own_vpn")) probe(network, url, expected)
                        else JSONObject().put("ok", false).put("failure", "OWN_VPN_CHANGED")
                    row.put("probe", p).put("end_utc", Instant.now().toString())
                    if (!p.getBoolean("ok")) probeFailures++
                    try {
                        val current = snapshot()
                        val same = current.installation == original.installation && current.registration == original.registration &&
                            current.grant == original.grant && current.generation == original.generation && current.slotsUsed == original.slotsUsed
                        val monotonic = current.seq.toBigInteger() >= previous.seq.toBigInteger() && current.revision.toBigInteger() >= previous.revision.toBigInteger()
                        identityOk = identityOk && same && monotonic && clockOk
                        row.put("same_identity_grant_generation_slots", same).put("versions_monotonic", monotonic)
                            .put("lease_seq", current.seq).put("revision", current.revision).put("pending", current.pending)
                            .put("expiry_utc", current.expiry.toString()).put("refresh_after_utc", current.refreshAfter.toString())
                            .put("catalog_expiry_utc", current.catalogExpiry.toString())
                        val fresh = Instant.now().isBefore(current.expiry) && Instant.now().isBefore(current.catalogExpiry)
                        row.put("fresh_catalog_lease", fresh)
                        identityOk = identityOk && fresh
                        renewed = renewed || (current.seq.toBigInteger() > original.seq.toBigInteger() && current.expiry > original.expiry)
                        afterOriginalExpiry = afterOriginalExpiry || (Instant.now() > original.expiry && fresh && p.getBoolean("ok"))
                        previous = current
                    } catch (_: Exception) { metadataFailures++; row.put("metadata_read_failed", true) }
                    row.put("relay", JSONObject(SessionService.lastRelay))
                    samples.put(row); checkpoint()
                    nextProbe = connected + ((SystemClock.elapsedRealtime() - connected) / 30_000 + 1) * 30_000
                    if (!identityOk) { result.put("reason", "IDENTITY_VERSION_OR_FRESHNESS_FAILED"); break }
                    if (now >= budget.holdEnd()) {
                        val keySame = MessageDigest.getInstance("SHA-256").digest(store.publicSpki()).contentEquals(key)
                        val full = budget.holdingDone(SystemClock.elapsedRealtime())
                        val pass = full && renewed && afterOriginalExpiry && keySame && probeFailures == 0 && metadataFailures == 0 && observe().phase == "Connected"
                        result.put("full_hold", full).put("same_key", keySame).put("renewed", renewed)
                            .put("probe_after_original_expiry", afterOriginalExpiry).put("passed", pass)
                            .put("reason", if (pass) "PASS" else "HOLD_OR_RENEWAL_PROBE_FAILED")
                        break
                    }
                }
                SystemClock.sleep(100)
            }
        } finally {
            result.put("hold_elapsed_ms", SystemClock.elapsedRealtime() - connected)
                .put("probe_failures", probeFailures).put("metadata_failures", metadataFailures)
                .put("hold_end_utc", Instant.now().toString()).put("running", false)
            close()
        }
    }
    override fun close() { connection.getAndSet(null)?.disconnect(); slot.close() }
}

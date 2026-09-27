package xyz.terlimo.test

import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.io.File
import java.net.URL
import java.time.Instant
import javax.net.ssl.HttpsURLConnection
import org.json.JSONObject
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

/**
 * S1-A01 evidence probe: uses the same app-side Java/HTTPS path as the S0 readiness
 * probe (HttpsURLConnection -> https://api.ipify.org) and reads back the persisted
 * routing policy (mode/revision/counts only, never rule values or secrets).
 *
 * Run while the real client is in the state under test (noVPN / connected / mode X).
 * Writes files/s1a01-route-evidence.json for pull.
 */
@RunWith(AndroidJUnit4::class)
class S1A01RouteEvidenceTest {
    @Test fun probeHttpsExitAndRoutingReadback() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val cm = context.getSystemService(ConnectivityManager::class.java)
        val vpnPresent = cm.allNetworks.any {
            cm.getNetworkCapabilities(it)?.hasTransport(NetworkCapabilities.TRANSPORT_VPN) == true
        }
        val exit: String = runCatching {
            val url = URL("https://api.ipify.org")
            val conn = url.openConnection() as HttpsURLConnection
            conn.connectTimeout = 12_000; conn.readTimeout = 12_000; conn.instanceFollowRedirects = false
            try {
                val status = conn.responseCode
                val body = conn.inputStream.readBytes().toString(Charsets.UTF_8).trim()
                if (status != 200) "STATUS_$status" else body
            } finally { conn.disconnect() }
        }.getOrElse { "ERROR_${it.javaClass.simpleName}" }

        val settings = runCatching {
            InstallationStore(context).readRoutingSettings(
                setOf(context.packageName), QuickExclusionCatalog(emptyList()))
        }.getOrNull()

        val out = JSONObject()
            .put("utc", Instant.now().toString())
            .put("vpn_present", vpnPresent)
            .put("exit_ip", exit)
            .put("routing_mode", settings?.routing?.apps?.mode?.name ?: "NONE")
            .put("routing_revision", settings?.revision ?: -1)
            .put("routing_count", settings?.routing?.apps?.packageNames?.size ?: -1)
            .put("summary", settings?.safeSummary() ?: "NONE")
        File(context.filesDir, "s1a01-route-evidence.json").writeText(out.toString())
        println("S1A01_EVIDENCE $out")
        // Probe must at least reach HTTPS (either TEST exit or direct exit).
        assertTrue("HTTPS probe failed: $exit", !exit.startsWith("ERROR_") && !exit.startsWith("STATUS_"))
    }
}

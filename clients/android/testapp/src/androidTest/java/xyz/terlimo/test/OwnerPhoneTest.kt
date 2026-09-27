package xyz.terlimo.test

import android.content.Intent
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.net.LinkProperties
import android.os.Build
import android.os.Bundle
import android.os.SystemClock
import android.view.View
import android.view.ViewGroup
import android.view.accessibility.AccessibilityNodeInfo
import android.widget.Button
import android.widget.EditText
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.json.JSONObject
import org.junit.Test
import org.junit.runner.RunWith
import java.io.File
import java.security.MessageDigest
import java.util.Base64

/** Explicit owner-authorized USB test only. No credentials in APK, arguments or output. */
@RunWith(AndroidJUnit4::class)
class OwnerPhoneTest {
    private fun linkFields(lp: LinkProperties?): Map<String, Any?>? = lp?.let {
        mapOf("INTERFACE" to it.interfaceName, "ADDRESSES" to it.linkAddresses.toSet(),
            "DNS" to it.dnsServers.toSet(), "ROUTES" to it.routes.toSet(), "DOMAINS" to it.domains,
            "MTU" to if (Build.VERSION.SDK_INT >= 29) it.mtu else null,
            "HTTP_PROXY" to it.httpProxy, "PRIVATE_DNS_ACTIVE" to it.isPrivateDnsActive,
            "PRIVATE_DNS_NAME" to it.privateDnsServerName,
            "NAT64" to if (Build.VERSION.SDK_INT >= 30) it.nat64Prefix else null)
    }
    private fun preservationSnapshot(): JSONObject {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        check(File(context.noBackupFilesDir, "installation.marker").isFile) { "TEST_EXISTING_KEY_REQUIRED" }
        val store = InstallationStore(context)
        val saved = store.read()
        val native = JSONObject(String(Base64.getUrlDecoder().decode(saved.getString("state_b64")), Charsets.UTF_8))
        val pending = native.getJSONObject("pending")
        check(pending.getString("op") == "register") { "TEST_PENDING_REGISTER_REQUIRED" }
        val request = Base64.getUrlDecoder().decode(pending.getString("request_id"))
        check(request.size == 16 && Base64.getUrlEncoder().withoutPadding().encodeToString(request) == pending.getString("request_id")) { "TEST_PENDING_ID_INVALID" }
        val payload = Base64.getUrlDecoder().decode(pending.getString("payload_b64"))
        val business = JSONObject(String(payload, Charsets.UTF_8))
        check(business.keys().asSequence().toSet() == setOf("op", "credential_id", "public_key_spki", "installation_id", "os")) { "TEST_PENDING_PAYLOAD_CONFLICT" }
        check(business.getString("op") == "register" && business.getString("os") == "android") { "TEST_PENDING_PAYLOAD_CONFLICT" }
        fun digest(bytes: ByteArray) = Base64.getEncoder().encodeToString(MessageDigest.getInstance("SHA-256").digest(bytes))
        return JSONObject().put("key", digest(store.publicSpki())).put("request", digest(request))
            .put("payload", digest(payload)).put("state", digest(saved.toString().toByteArray()))
    }

    @Test fun preserveExistingPendingBaseline() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val snapshot = preservationSnapshot()
        val baseline = File(context.noBackupFilesDir, "phone-test-baseline.json")
        if (baseline.exists()) {
            val prior = JSONObject(baseline.readText())
            for (key in listOf("key", "request", "payload", "state")) check(prior.getString(key) == snapshot.getString(key)) { "TEST_BASELINE_CONFLICT" }
        } else baseline.writeText(snapshot.toString())
        InstrumentationRegistry.getInstrumentation().sendStatus(2, Bundle().apply { putString("stage", "EXISTING_KEY_PENDING_PAYLOAD_VERIFIED") })
    }

    @Test fun firstConnectionFromPrivateFile() {
        if (InstrumentationRegistry.getArguments().getString("mode") == "selected-node") {
            SelectedNodeRun().run()
            return
        }
        val inst = InstrumentationRegistry.getInstrumentation()
        val context = inst.targetContext
        val result = JSONObject()
        val args = InstrumentationRegistry.getArguments()
        val testStart = SystemClock.elapsedRealtime()
        val testWall = System.currentTimeMillis()
        val stability = if (args.getString("stability_seconds") != null) {
            check(args.getString("mode") == "catalog" && args.getString("stability_seconds") == "1200") { "STABILITY_ARGUMENT_INVALID" }
            StabilityRun(context, args.getString("window_end_unix_ms")!!.toLong(), testStart, testWall)
        } else null
        stability?.let { result.put("stability", it.result) }
        val cm = context.getSystemService(ConnectivityManager::class.java)
        fun hasVpn() = cm.allNetworks.any { cm.getNetworkCapabilities(it)?.hasTransport(NetworkCapabilities.TRANSPORT_VPN) == true }
        val initialNetwork = cm.activeNetwork
        val initialProperties = initialNetwork?.let { cm.getLinkProperties(it) }
        val initiallyVpn = hasVpn()
        result.put("baseline_vpn", initiallyVpn)
        fun report(phase: String) {
            inst.sendStatus(2, Bundle().apply { putString("stage", phase) })
        }
        var last = ""
        fun observe(): ViewState {
            val state = SessionService.view
            if (last != state.phase) {
                last = state.phase
                report(state.phase)
                result.put("last_phase", state.phase)
            }
            state.error?.takeIf { it.matches(Regex("[A-Z][A-Z0-9_]{1,63}")) }?.let { result.put("error", it) }
            return state
        }
        fun views(v: View): List<View> = listOf(v) + if (v is ViewGroup) (0 until v.childCount).flatMap { views(v.getChildAt(it)) } else emptyList()
        // MIUI may deny a background launch from the instrumentation process.
        // Use the already authorized ADB-shell launch, without changing app permissions.
        val monitor = inst.addMonitor(MainActivity::class.java.name, null, false)
        inst.uiAutomation.executeShellCommand("am start -n xyz.terlimo.test/.MainActivity").close()
        val activity = inst.waitForMonitorWithTimeout(monitor, 15_000)
        inst.removeMonitor(monitor)
        check(activity != null) { "TEST_ACTIVITY_NOT_OPENED" }
        fun click(text: String) = inst.runOnMainSync {
            val button = views(activity.window.decorView).filterIsInstance<Button>().single { it.text.toString() == text }
            check(button.isEnabled) { "TEST_BUTTON_DISABLED" }
            button.performClick()
        }
        fun clickConnectWhenReady(deadline: Long, requestedNodeId: String?) {
            var clicked = false
            while (!clicked && SystemClock.elapsedRealtime() < deadline) {
                inst.runOnMainSync {
                    val state = SessionService.view
                    val selected = NodeSelection.connectableNodeId(state.nodes, state.selectedNodeId)
                    val spinner = views(activity.window.decorView).filterIsInstance<android.widget.Spinner>().single()
                    val displayed = NodeSelection.nodeIdAtSpinnerPosition(state.nodes, spinner.selectedItemPosition)
                    if (requestedNodeId != null && state.phase == "CatalogReady" && state.pendingNodeId == null &&
                        state.selectedNodeId != requestedNodeId) {
                        val position = state.nodes.indexOfFirst { it.id == requestedNodeId } + 1
                        check(position > 0) { "TEST_REQUESTED_NODE_MISSING" }
                        spinner.setSelection(position)
                    }
                    val button = views(activity.window.decorView).filterIsInstance<Button>()
                        .single { it.text.toString() == "Подключить выбранный сервер" }
                    if (state.phase == "CatalogReady" && selected != null &&
                        (requestedNodeId == null || selected == requestedNodeId) && displayed == selected && button.isEnabled) {
                        button.performClick()
                        clicked = true
                    }
                }
                if (!clicked) SystemClock.sleep(100)
            }
            check(clicked) { "TEST_CONNECT_NOT_READY" }
        }
        try {
            val mode = InstrumentationRegistry.getArguments().getString("mode")
            if (mode == "catalog") {
                check(File(context.noBackupFilesDir, "installation.marker").isFile) { "TEST_EXISTING_KEY_REQUIRED" }
                val store = InstallationStore(context)
                val baseline = JSONObject(File(context.noBackupFilesDir, "phone-test-baseline.json").readText())
                val keyDigest = Base64.getEncoder().encodeToString(MessageDigest.getInstance("SHA-256").digest(store.publicSpki()))
                check(baseline.getString("key") == keyDigest) { "TEST_KEY_CHANGED" }
                val saved = store.read()
                val native = JSONObject(String(Base64.getUrlDecoder().decode(saved.getString("state_b64")), Charsets.UTF_8))
                check(native.getJSONObject("catalog").getString("registration_id").isNotEmpty()) { "TEST_EXISTING_CATALOG_REQUIRED" }
                result.put("existing_registration_same_key", true)
                click("Открыть сохранённую подписку")
            } else if (mode == "resume") {
                val current = preservationSnapshot()
                val baseline = JSONObject(File(context.noBackupFilesDir, "phone-test-baseline.json").readText())
                for (key in listOf("key", "request", "payload", "state")) check(baseline.getString(key) == current.getString(key)) { "TEST_UPDATE_PRESERVATION_FAILED" }
                result.put("same_key_pending_payload_state", true)
                click("Открыть сохранённую подписку")
            } else {
            val input = File(context.filesDir, "owner-test-link")
            check(input.isFile && input.length() in 1..65536) { "TEST_INPUT_MISSING" }
            val link = input.readText().trim()
            check(input.delete()) { "TEST_INPUT_CLEANUP" }
            inst.runOnMainSync {
                views(activity.window.decorView).filterIsInstance<EditText>().single().setText(link)
            }
            click("Импортировать и получить каталог")
            result.put("private_import_handoff", true)
            }
            val bootstrapDeadline = minOf(SystemClock.elapsedRealtime() + 120_000, stability?.budget?.setupDeadline() ?: Long.MAX_VALUE)
            while (SystemClock.elapsedRealtime() < bootstrapDeadline) {
                val state = observe()
                if (state.phase in setOf("CatalogReady", "Error", "WaitingUser")) break
                SystemClock.sleep(250)
            }
            val catalog = observe().phase == "CatalogReady"
            result.put("catalog", catalog)
            if (catalog && !initiallyVpn) {
                val requested = if (mode == "catalog") {
                    args.getString("requested_node_id")
                        ?.takeIf { it.matches(Regex("[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}")) }
                        ?: error("TEST_REQUESTED_NODE_REQUIRED")
                } else null
                clickConnectWhenReady(minOf(SystemClock.elapsedRealtime() + 5_000, bootstrapDeadline), requested)
                val vpnDeadline = minOf(SystemClock.elapsedRealtime() + 120_000, stability?.budget?.setupDeadline() ?: Long.MAX_VALUE)
                while (SystemClock.elapsedRealtime() < vpnDeadline) {
                    // Only the OS VPN confirmation for the explicitly approved TEST connection.
                    val root = inst.uiAutomation.rootInActiveWindow
                    if (root?.packageName?.toString() == "com.android.vpndialogs") {
                        root.findAccessibilityNodeInfosByViewId("android:id/button1").firstOrNull()?.performAction(AccessibilityNodeInfo.ACTION_CLICK)
                    }
                    val state = observe()
                    if (state.phase in setOf("Connected", "Error", "WaitingUser")) break
                    SystemClock.sleep(250)
                }
                result.put("connected_readiness", observe().phase == "Connected")
                result.put("vpn_present", hasVpn())
                if (observe().phase == "Connected") {
                    if (stability == null) SystemClock.sleep(5000)
                    else stability.run(::observe) {
                        File(context.filesDir, "owner-test-result.json").writeText(result.toString())
                    }
                }
            } else if (initiallyVpn) result.put("vpn_test", "NOT_TESTED_EXISTING_VPN_PRESERVED")
        } finally {
            stability?.close()
            inst.runOnMainSync { context.startService(Intent(context, SessionService::class.java).setAction("cancel")) }
            val stopDeadline = SystemClock.elapsedRealtime() + 15_000
            while (SystemClock.elapsedRealtime() < stopDeadline && SessionService.view.phase !in setOf("Idle", "Error")) SystemClock.sleep(100)
            result.put("after_phase", SessionService.view.phase)
            result.put("vpn_restored", hasVpn() == initiallyVpn)
            result.put("default_network_restored", cm.activeNetwork == initialNetwork)
            val afterProperties = initialNetwork?.let { cm.getLinkProperties(it) }
            result.put("link_properties_restored", afterProperties == initialProperties)
            result.put("changed_link_property_fields", org.json.JSONArray(LinkPropertyDiff.changed(
                linkFields(initialProperties), linkFields(afterProperties), afterProperties == initialProperties)))
            result.put("readiness", JSONObject(SessionService.lastReadiness))
            result.put("relay", JSONObject(SessionService.lastRelay))
            result.put("bootstrap_diagnostic", JSONObject(SessionService.lastBootstrap))
            result.put("physical_network", JSONObject(SessionService.lastPhysicalNetwork))
            result.put("test_end_utc", java.time.Instant.now().toString())
            if (stability != null && (hasVpn() != initiallyVpn || SessionService.view.phase != "Idle" ||
                    cm.activeNetwork != initialNetwork || afterProperties != initialProperties)) {
                stability.result.put("passed", false).put("cleanup_ok", false)
            } else stability?.result?.put("cleanup_ok", true)
            File(context.filesDir, "owner-test-result.json").writeText(result.toString())
            report("RESULT_SAVED")
        }
    }
}

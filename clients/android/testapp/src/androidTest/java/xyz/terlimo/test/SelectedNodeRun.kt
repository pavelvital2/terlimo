package xyz.terlimo.test

import android.app.Activity
import android.content.Intent
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.NetworkCapabilities
import android.os.Build
import android.os.Bundle
import android.os.SystemClock
import android.util.AtomicFile
import android.view.View
import android.view.ViewGroup
import android.widget.Button
import android.widget.Spinner
import androidx.test.platform.app.InstrumentationRegistry
import org.json.JSONObject
import java.io.File
import java.security.MessageDigest
import java.time.Instant
import java.util.Base64

/** One bounded, explicit selected-node leg. It only drives the ordinary TEST UI. */
internal class SelectedNodeRun {
    private data class Identity(
        val key: ByteArray,
        val installation: String,
        val subscription: String,
        val registration: String,
    )

    private data class SelectedCatalog(
        val selectedNodeId: String,
        val requestedPresent: Boolean,
        val grantBound: Boolean,
        val pinValid: Boolean,
        val fresh: Boolean,
    )

    private fun linkFields(properties: LinkProperties?): Map<String, Any?>? = properties?.let {
        mapOf(
            "INTERFACE" to it.interfaceName,
            "ADDRESSES" to it.linkAddresses.toSet(),
            "DNS" to it.dnsServers.toSet(),
            "ROUTES" to it.routes.toSet(),
            "DOMAINS" to it.domains,
            "MTU" to if (Build.VERSION.SDK_INT >= 29) it.mtu else null,
            "HTTP_PROXY" to it.httpProxy,
            "PRIVATE_DNS_ACTIVE" to it.isPrivateDnsActive,
            "PRIVATE_DNS_NAME" to it.privateDnsServerName,
            "NAT64" to if (Build.VERSION.SDK_INT >= 30) it.nat64Prefix else null,
        )
    }

    fun run() {
        val inst = InstrumentationRegistry.getInstrumentation()
        val context = inst.targetContext
        val args = InstrumentationRegistry.getArguments()
        val startWall = System.currentTimeMillis()
        val startElapsed = SystemClock.elapsedRealtime()
        val result = JSONObject()
            .put("test_start_utc", Instant.ofEpochMilli(startWall).toString())
        val booleanFields = listOf(
            "baseline_vpn", "existing_registration_same_key", "catalog",
            "explicit_selection", "selected_node_matches", "selected_grant_binding",
            "selected_pin_valid", "selected_probe_provisioned", "selected_config_binding",
            "same_installation_key", "same_registration", "connected_readiness", "vpn_present",
            "vpn_restored", "default_network_restored", "link_properties_restored",
        )
        booleanFields.forEach { result.put(it, false) }

        val resultFile = File(context.filesDir, "owner-test-result.json")
        val cm = context.getSystemService(ConnectivityManager::class.java)
        fun hasVpn() = cm.allNetworks.any {
            cm.getNetworkCapabilities(it)?.hasTransport(NetworkCapabilities.TRANSPORT_VPN) == true
        }
        val initialNetwork = cm.activeNetwork
        val initialProperties = initialNetwork?.let(cm::getLinkProperties)
        val initiallyVpn = hasVpn()
        result.put("baseline_vpn", initiallyVpn)
        var original: Identity? = null
        var sameInstallation = false
        var sameRegistration = false
        var activity: Activity? = null
        var lastReported = ""
        var operationDeadline = startElapsed
        var attemptStarted = false
        var failure: Throwable? = null
        var store: InstallationStore? = null

        fun report(phase: String) {
            if (phase == lastReported) return
            lastReported = phase
            inst.sendStatus(2, Bundle().apply { putString("stage", phase) })
            result.put("last_phase", phase)
        }
        fun observe(): ViewState {
            val state = SessionService.view
            report(state.phase)
            state.error?.let { result.put("error", safeCode(it)) }
            return state
        }
        fun remaining() = operationDeadline - SystemClock.elapsedRealtime()
        fun requireTime() = check(remaining() > 0) { "OPERATION_DEADLINE" }
        fun identity(): Identity {
            val existing = store ?: error("TEST_EXISTING_KEY_REQUIRED")
            val native = nativeState(existing)
            val catalog = native.getJSONObject("catalog")
            val installation = native.getString("installation_id")
            val subscription = catalog.getString("subscription_ref")
            val registration = catalog.getString("registration_id")
            check(installation == existing.installationId()) { "TEST_INSTALLATION_BINDING_CHANGED" }
            check(subscription.isNotEmpty() && registration.isNotEmpty()) { "TEST_EXISTING_CATALOG_REQUIRED" }
            check(native.optJSONObject("pending")?.optString("op") != "register") {
                "TEST_PENDING_REGISTER_FORBIDDEN"
            }
            return Identity(existing.publicSpki(), installation, subscription, registration)
        }
        fun observeIdentity() {
            val current = identity()
            val first = original ?: return
            sameInstallation = sameInstallation && current.key.contentEquals(first.key) &&
                current.installation == first.installation
            sameRegistration = sameRegistration && current.subscription == first.subscription &&
                current.registration == first.registration
            result.put("same_installation_key", sameInstallation)
                .put("same_registration", sameRegistration)
            check(sameInstallation) { "TEST_KEY_CHANGED" }
            check(sameRegistration) { "TEST_REGISTRATION_CHANGED" }
        }

        try {
            check(!resultFile.exists() || resultFile.delete()) { "OLD_RESULT_NOT_REMOVED" }
            val requested = safeArgument(args.getString("requested_node_id"), "REQUESTED_NODE_ID_INVALID")
            val runId = safeArgument(args.getString("run_id"), "RUN_ID_INVALID")
            val scenarioId = safeArgument(args.getString("scenario_id"), "SCENARIO_ID_INVALID")
            val legIndex = args.getString("leg_index")?.toIntOrNull()
            check(legIndex == 1 || legIndex == 2) { "LEG_INDEX_INVALID" }
            val operationEnd = args.getString("operation_end_unix_ms")?.let { raw ->
                check(raw.matches(Regex("[0-9]{13}"))) { "OPERATION_END_INVALID" }
                raw.toLong()
            } ?: error("OPERATION_END_INVALID")
            val operationBudget = operationEnd - startWall
            check(operationBudget in 1..200_000) { "OPERATION_END_INVALID" }
            operationDeadline = startElapsed + operationBudget
            result.put("run_id", runId).put("requested_node_id", requested)

            check(!initiallyVpn) { "TEST_VPN_ALREADY_PRESENT" }
            // InstallationStore can ensure identity on first key access. Verify the
            // existing-install marker before constructing/using it at all.
            check(File(context.noBackupFilesDir, "installation.marker").isFile) {
                "TEST_EXISTING_KEY_REQUIRED"
            }
            store = InstallationStore(context)
            val startIdentity = identity()
            original = startIdentity
            val ownerBaseline = File(context.noBackupFilesDir, "phone-test-baseline.json")
            check(ownerBaseline.isFile) { "TEST_BASELINE_MISSING" }
            val oldBaseline = JSONObject(ownerBaseline.readText())
            val currentKeyDigest = digest(startIdentity.key)
            check(oldBaseline.getString("key") == currentKeyDigest) { "TEST_KEY_CHANGED" }
            val scenarioFile = File(context.noBackupFilesDir, "selected-node-$scenarioId-baseline.json")
            val scenario = identityDigests(startIdentity)
            if (scenarioFile.exists()) {
                check(scenarioFile.isFile && sameDigests(JSONObject(scenarioFile.readText()), scenario)) {
                    "TEST_SCENARIO_IDENTITY_CHANGED"
                }
            } else {
                check(legIndex == 1) { "TEST_SCENARIO_BASELINE_MISSING" }
                writeAtomic(scenarioFile, scenario.toString())
            }
            sameInstallation = true
            sameRegistration = true
            result.put("existing_registration_same_key", true)
                .put("same_installation_key", true)
                .put("same_registration", true)

            requireTime()
            val monitor = inst.addMonitor(MainActivity::class.java.name, null, false)
            inst.uiAutomation.executeShellCommand("am start -n xyz.terlimo.test/.MainActivity").close()
            activity = inst.waitForMonitorWithTimeout(monitor, minOf(15_000L, remaining().coerceAtLeast(1)))
            inst.removeMonitor(monitor)
            check(activity != null) { "TEST_ACTIVITY_NOT_OPENED" }
            requireTime()
            check(SessionService.view.phase == "Idle") { "TEST_SESSION_NOT_IDLE" }
            attemptStarted = true
            click(activity!!, "Открыть сохранённую подписку", inst)

            val ready = waitFor(operationDeadline) {
                val state = observe()
                state.phase == "CatalogReady" || state.phase == "Error" || state.phase == "WaitingUser"
            }
            check(ready && observe().phase == "CatalogReady") { "TEST_CATALOG_NOT_READY" }
            result.put("catalog", true)
            observeIdentity()

            val initialCatalog = selectedCatalog(store!!, requested)
            check(initialCatalog.fresh) { "TEST_CATALOG_NOT_FRESH" }
            check(initialCatalog.requestedPresent && SessionService.view.nodes.any { it.id == requested }) {
                "TEST_REQUESTED_NODE_MISSING"
            }
            result.put("selected_grant_binding", initialCatalog.grantBound)
                .put("selected_pin_valid", initialCatalog.pinValid)
            check(initialCatalog.grantBound) { "TEST_SELECTED_GRANT_BINDING" }
            check(initialCatalog.pinValid) { "TEST_SELECTED_PIN_INVALID" }

            val probeSettings = NodeProbeSettings.parse(
                context.assets.open("test-probe.json").bufferedReader().use { it.readText() },
                context.assets.open("test-public-trust.json").bufferedReader().use { it.readText() },
            )
            val probeProvisioned = probeSettings[requested] != null
            result.put("selected_probe_provisioned", probeProvisioned)
            check(probeProvisioned) { "TEST_SELECTED_PROBE_MISSING" }

            requireTime()
            val position = SessionService.view.nodes.indexOfFirst { it.id == requested } + 1
            check(position > 0) { "TEST_REQUESTED_NODE_MISSING" }
            inst.runOnMainSync {
                val spinner = views(activity!!.window.decorView).filterIsInstance<Spinner>().single()
                check(position < spinner.adapter.count) { "TEST_SPINNER_ID_MISMATCH" }
                check(NodeSelection.nodeIdAtSpinnerPosition(SessionService.view.nodes, position) == requested) {
                    "TEST_SPINNER_ID_MISMATCH"
                }
                spinner.setSelection(position)
                check(spinner.selectedItemPosition == position) { "TEST_SPINNER_ID_MISMATCH" }
            }
            result.put("explicit_selection", true)

            val selected = waitFor(operationDeadline) {
                val state = observe()
                if (state.phase == "CatalogReady" && state.nodes.none { it.id == requested })
                    error("TEST_REQUESTED_NODE_REMOVED")
                state.phase == "Error" || state.phase == "WaitingUser" ||
                    (state.phase == "CatalogReady" && state.selectedNodeId == requested && state.pendingNodeId == null)
            }
            check(selected && observe().phase == "CatalogReady") { "TEST_SELECTION_NOT_ACKNOWLEDGED" }
            val selectedCatalog = selectedCatalog(store!!, requested)
            val selectedMatches = selectedCatalog.selectedNodeId == requested && selectedCatalog.requestedPresent &&
                SessionService.view.selectedNodeId == requested && SessionService.view.pendingNodeId == null
            result.put("selected_node_matches", selectedMatches)
                .put("selected_grant_binding", selectedCatalog.grantBound)
                .put("selected_pin_valid", selectedCatalog.pinValid)
            check(selectedMatches) { "TEST_SELECTION_NOT_DURABLE" }
            check(selectedCatalog.grantBound && selectedCatalog.pinValid) { "TEST_SELECTED_BINDING_CHANGED" }
            observeIdentity()

            requireTime()
            click(activity!!, "Подключить выбранный сервер", inst)
            val connected = waitFor(operationDeadline) {
                val state = observe()
                if (inst.uiAutomation.rootInActiveWindow?.packageName?.toString() == "com.android.vpndialogs")
                    error("TEST_VPN_CONSENT_REQUIRED")
                state.phase == "Connected" || state.phase == "Error" || state.phase == "WaitingUser"
            }
            val connectedState = connected && observe().phase == "Connected"
            result.put("connected_readiness", connectedState).put("vpn_present", hasVpn())
            check(connectedState && hasVpn()) { "TEST_SELECTED_NODE_NOT_CONNECTED" }
            val connectedCatalog = selectedCatalog(store!!, requested)
            val connectedMatches = connectedCatalog.selectedNodeId == requested && connectedCatalog.requestedPresent &&
                SessionService.view.selectedNodeId == requested && SessionService.view.pendingNodeId == null
            result.put("selected_node_matches", connectedMatches)
                .put("selected_grant_binding", connectedCatalog.grantBound)
                .put("selected_pin_valid", connectedCatalog.pinValid)
            check(connectedMatches && connectedCatalog.grantBound && connectedCatalog.pinValid) {
                "TEST_SELECTED_BINDING_CHANGED"
            }
            observeIdentity()
            // Connected is published only after SessionService's mandatory exact node,
            // immutable probe mapping, WireGuard config and readiness gates all pass.
            val selectedConfigBinding = connectedMatches && probeProvisioned &&
                SessionService.lastReadiness["ready"] == true
            result.put("selected_config_binding", selectedConfigBinding)
            check(selectedConfigBinding) { "TEST_SELECTED_CONFIG_BINDING" }
        } catch (error: Throwable) {
            failure = error
            if (!result.has("error")) result.put("error", safeCode(error.message))
        } finally {
            var cleanupRequestOk = true
            if (attemptStarted) {
                cleanupRequestOk = runCatching {
                    inst.runOnMainSync {
                        context.startService(Intent(context, SessionService::class.java).setAction("cancel"))
                    }
                }.isSuccess
                val cleanupEnd = SystemClock.elapsedRealtime() + 15_000
                while (SystemClock.elapsedRealtime() < cleanupEnd &&
                    SessionService.view.phase !in setOf("Idle", "Error")) {
                    SystemClock.sleep(100)
                }
            }
            val afterProperties = initialNetwork?.let(cm::getLinkProperties)
            val finalPhase = SessionService.view.phase
            result.put("after_phase", finalPhase)
                .put("vpn_restored", hasVpn() == initiallyVpn)
                .put("default_network_restored", cm.activeNetwork == initialNetwork)
                .put("link_properties_restored", afterProperties == initialProperties)
                .put("changed_link_property_fields", org.json.JSONArray(LinkPropertyDiff.changed(
                    linkFields(initialProperties), linkFields(afterProperties), afterProperties == initialProperties)))
                .put("readiness", JSONObject(SessionService.lastReadiness))
                .put("relay", JSONObject(SessionService.lastRelay))
                .put("bootstrap_diagnostic", JSONObject(SessionService.lastBootstrap))
                .put("physical_network", JSONObject(SessionService.lastPhysicalNetwork))
            if (attemptStarted && (!cleanupRequestOk || finalPhase != "Idle") && !result.has("error")) {
                result.put("error", "TEST_CLEANUP_FAILED")
            }
            if (original != null) runCatching { observeIdentity() }.onFailure {
                result.put("same_installation_key", false).put("same_registration", false)
                if (!result.has("error")) result.put("error", safeCode(it.message))
            }
            result.put("test_end_utc", Instant.now().toString())
            resultFile.writeText(result.toString())
            report("RESULT_SAVED")
        }
        failure?.let { throw it }
    }

    private fun selectedCatalog(store: InstallationStore, requested: String): SelectedCatalog {
        val root = nativeState(store)
        val catalog = root.getJSONObject("catalog")
        val registration = catalog.getString("registration_id")
        val nodes = catalog.getJSONArray("nodes")
        var requestedNode: JSONObject? = null
        repeat(nodes.length()) { index ->
            val node = nodes.getJSONObject(index)
            if (node.getString("node_id") == requested) {
                check(requestedNode == null) { "TEST_DUPLICATE_REQUESTED_NODE" }
                requestedNode = node
            }
        }
        val node = requestedNode
        val access = node?.optJSONObject("access")
        val pin = node?.optString("dtls_spki_sha256").orEmpty()
        val pinValid = runCatching {
            val decoded = Base64.getUrlDecoder().decode(pin)
            decoded.size == 32 && Base64.getUrlEncoder().withoutPadding().encodeToString(decoded) == pin
        }.getOrDefault(false)
        val fresh = runCatching { Instant.parse(catalog.getString("catalog_expires_at")).isAfter(Instant.now()) }
            .getOrDefault(false)
        return SelectedCatalog(root.optString("selected_node_id"), node != null,
            access?.optString("device_id") == registration, pinValid, fresh)
    }

    private fun nativeState(store: InstallationStore): JSONObject {
        val saved = store.read()
        val bytes = Base64.getUrlDecoder().decode(saved.getString("state_b64"))
        return try { JSONObject(bytes.toString(Charsets.UTF_8)) } finally { bytes.fill(0) }
    }

    private fun identityDigests(identity: Identity) = JSONObject()
        .put("key", digest(identity.key))
        .put("installation", digest(identity.installation.toByteArray(Charsets.UTF_8)))
        .put("subscription", digest(identity.subscription.toByteArray(Charsets.UTF_8)))
        .put("registration", digest(identity.registration.toByteArray(Charsets.UTF_8)))

    private fun sameDigests(left: JSONObject, right: JSONObject): Boolean {
        val fields = setOf("key", "installation", "subscription", "registration")
        return left.keys().asSequence().toSet() == fields && fields.all { left.optString(it) == right.getString(it) }
    }

    private fun digest(bytes: ByteArray): String = Base64.getEncoder().encodeToString(
        MessageDigest.getInstance("SHA-256").digest(bytes))

    private fun writeAtomic(file: File, value: String) {
        val atomic = AtomicFile(file)
        val output = atomic.startWrite()
        try {
            output.write(value.toByteArray(Charsets.UTF_8))
            atomic.finishWrite(output)
        } catch (error: Throwable) {
            atomic.failWrite(output)
            throw error
        }
    }

    private fun safeArgument(value: String?, code: String): String {
        check(value != null && value.matches(Regex("[A-Za-z0-9][A-Za-z0-9._:-]{0,127}"))) { code }
        return value
    }

    private fun safeCode(value: String?): String =
        value?.takeIf { it.matches(Regex("[A-Z][A-Z0-9_]{0,63}")) } ?: "TEST_HARNESS_FAILED"

    private fun waitFor(endElapsed: Long, condition: () -> Boolean): Boolean {
        while (SystemClock.elapsedRealtime() < endElapsed) {
            if (condition()) return true
            SystemClock.sleep(100)
        }
        return false
    }

    private fun views(view: View): List<View> = listOf(view) +
        if (view is ViewGroup) (0 until view.childCount).flatMap { views(view.getChildAt(it)) } else emptyList()

    private fun click(activity: Activity, text: String, inst: android.app.Instrumentation) = inst.runOnMainSync {
        val button = views(activity.window.decorView).filterIsInstance<Button>().single { it.text.toString() == text }
        check(button.isEnabled) { "TEST_BUTTON_DISABLED" }
        check(button.performClick()) { "TEST_BUTTON_CLICK_FAILED" }
    }
}

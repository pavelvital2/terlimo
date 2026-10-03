package xyz.terlimo.test

import java.io.File
import java.security.MessageDigest
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

/**
 * STEP03.6 owner contract "catalog before access": host-side browse branch, host-local
 * selection, optional gateway_key carry, conflict/offline mapping and the once-per-process
 * auto-load. The credential catalog and connect paths are pinned as unchanged.
 */
class BrowseOwnerContractTest {
    private fun source(path: String): String =
        listOf(File(path), File("testapp/$path"), File("../$path")).first { it.isFile }.readText()

    private val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
    private val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")
    private val catalogView = source("src/main/java/xyz/terlimo/test/ServerCatalogView.kt")
    private val orbit = source("src/main/java/xyz/terlimo/test/OrbitHomeHeader.kt")

    private fun browseEvent(vararg nodes: Pair<String, String>): JSONObject = JSONObject()
        .put("v", 1).put("attempt_id", "attempt-a").put("type", "catalog").put("catalog_mode", "browse")
        .put("nodes", JSONArray().apply {
            nodes.forEachIndexed { index, (id, name) ->
                put(JSONObject().put("node_id", id).put("name", name)
                    .put("country_code", if (index % 2 == 0) "DE" else "").put("region", "eu"))
            }
        })
        .put("issued_at", "2026-09-23T20:00:00Z")
        .put("catalog_expires_at", "2026-09-23T20:10:00Z")

    private fun projection(
        dataAccess: String = "none",
        onboarding: String = "not_started",
    ) = AccountAccessProjection(
        accessVersion = 1,
        serverTime = "2026-09-23T20:00:00Z",
        sessionGeneration = "1",
        previousSessionGeneration = null,
        accessRevision = "1",
        account = AccountAccessProjection.AccountAccessAccount(
            state = "UNLINKED", telegramLinked = false, bindingStatus = "none",
            managementOnly = false, accountRef = null),
        entitlement = AccountAccessProjection.AccountAccessEntitlement(
            type = "none", status = "none", effectiveDeviceLimit = 1, slotsUsed = 0,
            revision = "1", perpetualCommercial = false, validFrom = null, validUntil = null, sourceRef = null),
        onboarding = AccountAccessProjection.AccountAccessOnboarding(
            state = onboarding, startedBy = "server_confirmed_first_connection", startedAt = null,
            notAfter = null, durationSeconds = 3600, oneTime = true, extendsOnRefresh = false,
            extendsOnRestart = false, createsTrial = false, requiresHardwareId = false,
            unit = "installation_fingerprint", postTelegramIdentity = "account_history_correlation",
            preTelegramReinstall = "may_be_indistinguishable_new_key_separate_unit"),
        grant = AccountAccessProjection.AccountAccessGrant(
            controlAvailable = true, restrictedCheckoutAvailable = true,
            dataAccess = dataAccess, effectiveDeadline = null),
    )

    private fun waitingState() = ViewState(
        phase = "BootstrapConnecting",
        accountAccess = AccountAccessSnapshot(projection(), 0L, AccountAccessChain()),
    )

    private fun browseState(selection: String = "") = BrowseCatalogCodec.apply(
        waitingState(),
        BrowseCatalog(listOf(NodeLabel("gw-1", "Первый", "DE"), NodeLabel("gw-2", "Второй", ""))),
    ).copy(browseSelectedId = selection)

    @Test
    fun `browse parses zero and three nodes and never becomes verified state`() {
        assertTrue(BrowseCatalogCodec.parse(browseEvent()).nodes.isEmpty())
        val three = BrowseCatalogCodec.parse(browseEvent("gw-1" to "Один", "gw-2" to "Два", "gw-3" to "Три"))
        assertEquals(listOf("gw-1", "gw-2", "gw-3"), three.nodes.map { it.id })
        assertEquals(listOf("DE", "", "DE"), three.nodes.map { it.countryCode })

        val verified = ViewState(phase = "CatalogReady", nodes = listOf(NodeLabel("v", "Verified", "FI")),
            selectedNodeId = "v", catalogRevision = "9", pings = mapOf("v" to NodePingState.Timeout))
        val applied = BrowseCatalogCodec.apply(verified, three)
        assertEquals(three.nodes, applied.browseNodes)
        assertTrue(applied.browseLoaded)
        assertEquals(CatalogDisplayMode.BROWSE, applied.displayMode)
        // The verified catalog, its selection, revision and pings stay byte-for-byte the same.
        assertEquals("CatalogReady", applied.phase)
        assertEquals(verified.nodes, applied.nodes)
        assertEquals("v", applied.selectedNodeId)
        assertEquals("9", applied.catalogRevision)
        assertEquals(verified.pings, applied.pings)
        assertNull(applied.browseError)
        // A waiting attempt stays waiting: browse never becomes CatalogReady/connectable.
        assertEquals("BootstrapConnecting", BrowseCatalogCodec.apply(waitingState(), three).phase)
    }

    @Test
    fun `browse branch precedes the strict decoder and writes no verified state`() {
        val handler = service.substringAfter("\"catalog\" -> {").substringBefore("\"node_probe_result\" ->")
        assertTrue(handler.indexOf("!catalogTimer.canAccept(attempt, pendingCycle)") < handler.indexOf("catalogGate.commit"))
        val browseAt = handler.indexOf("BrowseCatalogCodec.isBrowse(event)")
        val decodeAt = handler.indexOf("NodeSelection.parseCatalog(event)")
        assertTrue("browse must be decided before the credential decoder", browseAt in 1 until decodeAt)
        val browsePath = handler.substring(browseAt, decodeAt)
        listOf("persistCatalogCache", "NodeSelection.applyCatalog",
            "probe_node", "select_node", "autoConnectRetained").forEach {
            assertFalse(it, browsePath.contains(it))
        }
        assertTrue(browsePath.contains("if (refreshPlan.publish)"))
        assertTrue(browsePath.contains("CatalogTimerMarker.ACCEPT"))
        assertTrue(browsePath.contains("MobileCatalogAction.DISARM"))
        val accessPath = service.substringAfter("\"account_access\" -> {")
            .substringBefore("\"catalog_stage\" -> {")
        assertFalse(accessPath.contains("mobileCatalog.onAccountAccess"))
        // The credential path keeps its existing shape.
        assertTrue(handler.contains("NodeSelection.applyCatalog(view, catalog, summary)"))
        assertTrue(handler.contains("runCatching { persistCatalogCache(updated) }"))
        assertTrue(handler.contains("mobileCatalog.onCatalogAccepted(attempt)"))
        assertEquals(3, Regex("catalogTimer\\.apply\\(").findAll(service).count())
    }

    @Test
    fun `browse decode rejects malformed metadata`() {
        fun event(vararg node: JSONObject) = JSONObject().put("catalog_mode", "browse")
            .put("nodes", JSONArray(node.toList()))
        assertThrows(IllegalStateException::class.java) { BrowseCatalogCodec.parse(event(
            JSONObject().put("node_id", "gw-1").put("name", "A").put("country_code", "DEUTSCHLA"))) }
        assertThrows(IllegalStateException::class.java) { BrowseCatalogCodec.parse(event(
            JSONObject().put("node_id", "gw-1").put("name", "A").put("country_code", 5))) }
        assertThrows(IllegalStateException::class.java) { BrowseCatalogCodec.parse(event(
            JSONObject().put("node_id", "gw-1").put("name", "A").put("region", "r".repeat(65)))) }
        assertThrows(IllegalStateException::class.java) { BrowseCatalogCodec.parse(event(
            JSONObject().put("node_id", "gw-1").put("name", "A").put("country_code", "DE"),
            JSONObject().put("node_id", "gw-1").put("name", "B").put("country_code", "DE"))) }
        assertThrows(IllegalStateException::class.java) { BrowseCatalogCodec.parse(event(
            JSONObject().put("node_id", "gw-1").put("country_code", "DE"))) }
        assertFalse(BrowseCatalogCodec.isBrowse(JSONObject().put("type", "catalog")))
        // Go-aligned bounds: any country up to 8 chars is display metadata, and absent or
        // null country_code/region are accepted exactly like the Go browse decode
        // (authoritative fixture gw-beta carries neither).
        val metadata = BrowseCatalogCodec.parse(event(
            JSONObject().put("node_id", "gw-1").put("name", "A").put("country_code", "DEU"),
            JSONObject().put("node_id", "gw-2").put("name", "B"),
            JSONObject().put("node_id", "gw-3").put("name", "C")
                .put("country_code", JSONObject.NULL).put("region", JSONObject.NULL)))
        assertEquals(listOf("DEU", "", ""), metadata.nodes.map { it.countryCode })
    }

    @Test
    fun `step036 synthetic fixtures match the pinned server hashes`() {
        val pinned = mapOf(
            "step036_catalog_browse.json" to "750ad25b84997c2a6c8e19a7c566e54eaadad7c42bffc0a6a3e2f0626b205937",
            "step036_catalog_credential.json" to "518a2f9605d6b2fcb0bf94fbac37c83e2af41fd2f5a23d9b0a0d7baf72736ae8",
            "step036_intent_payload_gateway_key.json" to "25d74d343883cb8d289acd10980bb8351e67ea8503bb6087541cdd8556e0521c",
        )
        pinned.forEach { (name, sha256) ->
            val raw = javaClass.getResourceAsStream("/step036/$name")!!.use { it.readBytes() }
            val sum = MessageDigest.getInstance("SHA-256").digest(raw).joinToString("") { "%02x".format(it) }
            assertEquals(name, sha256, sum)
        }
    }

    @Test
    fun `browse parses the authoritative fixture with the missing-country node accepted`() {
        val raw = javaClass.getResourceAsStream("/step036/step036_catalog_browse.json")!!
            .use { it.readBytes().toString(Charsets.UTF_8) }
        val marker = "\"synthetic\": true,"
        assertTrue(raw.contains(marker))
        val wire = JSONObject(raw.replaceFirst(marker, ""))
        assertEquals("browse", wire.getString("catalog_mode"))
        val gateways = wire.getJSONArray("gateways")
        assertEquals(3, gateways.length())
        // Project the wire gateways exactly like the native bridge (gateway_id -> node_id)
        // and keep absent metadata absent: the host parser must align with the Go decode.
        val event = JSONObject().put("catalog_mode", "browse").put("nodes", JSONArray().apply {
            repeat(gateways.length()) { index ->
                val gateway = gateways.getJSONObject(index)
                val node = JSONObject().put("node_id", gateway.getString("gateway_id"))
                    .put("name", gateway.getString("name"))
                if (gateway.has("country_code")) node.put("country_code", gateway.getString("country_code"))
                if (gateway.has("region")) node.put("region", gateway.getString("region"))
                put(node)
            }
        })
        assertTrue(BrowseCatalogCodec.isBrowse(event))
        val parsed = BrowseCatalogCodec.parse(event)
        assertEquals(listOf("gw-alpha", "gw-beta", "gw-gamma"), parsed.nodes.map { it.id })
        assertEquals(listOf("XX", "", "DE"), parsed.nodes.map { it.countryCode })
        assertEquals("BETA", parsed.nodes[1].name)
    }

    @Test
    fun `browse selection is host-local and never probes or talks to native`() {
        assertTrue(BrowseCatalogCodec.displayedNodes(browseState()).isNotEmpty())
        assertEquals("", BrowseCatalogCodec.selectedId(browseState()))
        assertEquals("gw-2", BrowseCatalogCodec.selectedId(browseState("gw-2")))
        // A preference naming a row that is not displayed is never exposed.
        assertEquals("", BrowseCatalogCodec.selectedId(browseState("gone")))
        // A fresh credential catalog answer switches back to the credential display and
        // drops/ignores the browse rows and their preference.
        val credential = NodeSelection.applyCatalog(browseState("gw-2"),
            NodeCatalog(listOf(NodeLabel("v", "V", "FI")), "v", "9"), null)
        assertEquals(CatalogDisplayMode.CREDENTIAL, credential.displayMode)
        assertTrue(BrowseCatalogCodec.displayedNodes(credential).isEmpty())
        assertEquals("", BrowseCatalogCodec.selectedId(credential))
        assertTrue(credential.browseNodes.isEmpty())
        assertFalse(credential.browseLoaded)
        assertNull(credential.browseError)

        val handler = service.substringAfter("\"browse_select\" ->").substringBefore("\"onboarding_connect\" ->")
        assertTrue(handler.contains("BrowseCatalogCodec.displayedNodes(view).any { it.id == id }"))
        listOf("native?.send", "\"select_node\"", "\"explicit_connect\"", "explicitConnect.arm(").forEach {
            assertFalse(it, handler.contains(it))
        }

        val activitySelection = activity.substringAfter("private fun requestNodeSelection(")
            .substringBefore("private fun projectedState")
        assertTrue(activitySelection.contains("BrowseCatalogCodec.displayedNodes(state).any { it.id == chosen }"))
        assertTrue(activitySelection.contains("setAction(\"browse_select\")"))
        assertTrue(activitySelection.indexOf("browse_select") < activitySelection.indexOf("val switchPhase"))

        val row = catalogView.substringAfter("private fun addBrowseRow(").substringBefore("private fun row(")
        assertTrue(row.contains("setOnClickListener { onSelect(node.id) }"))
        assertFalse(row.contains("onProbe"))
        assertFalse(row.contains("Button("))
        assertFalse(row.contains("Ping"))
    }

    @Test
    fun `connect needs a browse selection and carries the chosen gateway key`() {
        assertTrue(BrowseConnectGate.requiresSelection(browseState()))
        assertFalse(BrowseConnectGate.connectable(browseState(), pendingChoice = false))
        assertFalse(BrowseConnectGate.connectable(browseState(), pendingChoice = true))
        assertFalse(BrowseConnectGate.requiresSelection(browseState("gw-2")))
        assertTrue(BrowseConnectGate.connectable(browseState("gw-2"), pendingChoice = false))

        // Browse loading (no answer yet): the pre-admission connect is blocked and prompted.
        val loading = waitingState().copy(displayMode = CatalogDisplayMode.BROWSE)
        assertTrue(BrowseConnectGate.requiresSelection(loading))
        assertFalse(BrowseConnectGate.connectable(loading, pendingChoice = false))
        // A successful empty browse answer has nothing to select: Connect stays blocked.
        val empty = BrowseCatalogCodec.apply(waitingState(), BrowseCatalog(emptyList()))
        assertTrue(BrowseConnectGate.requiresSelection(empty))
        assertFalse(BrowseConnectGate.connectable(empty, pendingChoice = false))
        // An offline browse error is still the browse display: Connect stays blocked.
        val errored = waitingState().copy(displayMode = CatalogDisplayMode.BROWSE, browseError = "TRANSPORT")
        assertTrue(BrowseConnectGate.requiresSelection(errored))
        assertFalse(BrowseConnectGate.connectable(errored, pendingChoice = false))
        // Legacy credential-mode callers keep the exact no-key behavior.
        assertFalse(BrowseConnectGate.requiresSelection(waitingState()))
        assertTrue(BrowseConnectGate.connectable(waitingState(), pendingChoice = false))
        // Verified/legacy states are never gated by a browse selection.
        assertFalse(BrowseConnectGate.requiresSelection(
            ViewState(phase = "CatalogReady", nodes = listOf(NodeLabel("v", "V", "FI")), selectedNodeId = "v")))

        assertTrue(activity.contains("putExtra(\"gateway_key\", gatewayKey)"))
        // The key stays absent for a legitimate caller without a selection (API compatibility).
        assertTrue(activity.contains("if (gatewayKey.isNotEmpty()) intent.putExtra(\"gateway_key\", gatewayKey)"))
        val command = service.substringAfter("private fun explicitConnectCommand(")
            .substringBefore("private fun stopAttempt")
        assertTrue(command.contains(".put(\"type\", \"explicit_connect\")"))
        assertTrue(command.contains("message.put(\"gateway_key\", gatewayKey)"))
        assertTrue(service.contains("native?.send(explicitConnectCommand(gatewayKey))"))
        assertTrue(service.contains("child.send(explicitConnectCommand(gatewayKey))"))
        // Selection itself never arms the explicit gate or starts a connect.
        val selection = service.substringAfter("\"browse_select\" ->").substringBefore("\"onboarding_connect\" ->")
        assertFalse(selection.contains("explicitConnect.arm("))
        assertFalse(selection.contains("begin("))
        // A browse prompt/disable exists before the consent funnel is reached, and the
        // service refuses a keyless explicit connect while a browse selection is required.
        assertTrue(activity.contains("BrowseConnectGate.requiresSelection(state)"))
        assertTrue(activity.contains("if (BrowseConnectGate.requiresSelection(SessionService.view))"))
        assertTrue(service.contains("if (BrowseConnectGate.requiresSelection(view))"))
        assertTrue(orbit.contains("BrowseConnectGate.requiresSelection(state)"))
        assertTrue(orbit.contains("\"Выберите сервер из списка для подключения\""))
        assertTrue(orbit.contains("\"Загружаем список серверов…\""))
        assertTrue(orbit.contains("\"Список серверов недоступен. Обновите список.\""))
        assertTrue(orbit.contains("\"Список серверов пуст. Обновите список.\""))
    }

    @Test
    fun `display mode separates a fresh browse answer from stale verified credentials`() {
        // Access(catalog) -> expired(browse): stale verified nodes no longer hide the fresh
        // browse list, which is the current display.
        val stale = ViewState(phase = "CatalogReady",
            nodes = listOf(NodeLabel("v", "Verified", "FI")), selectedNodeId = "v", catalogRevision = "9")
        val browsed = BrowseCatalogCodec.apply(stale, BrowseCatalog(listOf(NodeLabel("gw-1", "Первый", "DE"))))
        assertEquals(CatalogDisplayMode.BROWSE, browsed.displayMode)
        assertEquals(listOf("gw-1"), BrowseCatalogCodec.displayedNodes(browsed).map { it.id })
        assertTrue(BrowseCatalogCodec.isDisplayed(browsed))
        // The stale verified rows and cache material are untouched by the browse projection.
        assertEquals(stale.nodes, browsed.nodes)
        assertEquals("v", browsed.selectedNodeId)
        assertEquals("9", browsed.catalogRevision)

        // Browse -> credential: a fresh credential answer restores the credential display.
        val restored = NodeSelection.applyCatalog(browsed, NodeCatalog(browsed.nodes, "v", "10"), null)
        assertEquals(CatalogDisplayMode.CREDENTIAL, restored.displayMode)
        assertTrue(BrowseCatalogCodec.displayedNodes(restored).isEmpty())
        assertFalse(BrowseCatalogCodec.isDisplayed(restored))
        assertEquals(listOf("v"), BrowseCatalogCodec.verifiedNodes(restored).map { it.id })

        // Connectability is never armed by stale verified rows during browse.
        val staleIdle = stale.copy(phase = "Idle", displayMode = CatalogDisplayMode.BROWSE)
        assertNull(RetainedCatalogPolicy.connectableId(staleIdle))
        assertEquals("v", RetainedCatalogPolicy.connectableId(staleIdle.copy(displayMode = CatalogDisplayMode.CREDENTIAL)))
        assertTrue(BrowseCatalogCodec.verifiedNodes(staleIdle).isEmpty())

        // A durable retained cache never hydrates into a browse display, while the
        // credential retained path stays exactly as before.
        val raw = CatalogCacheCodec.encode(RetainedCatalog(listOf(NodeLabel("c", "Cached", "DE")), "c", "7"))
        val browseLoading = ViewState(displayMode = CatalogDisplayMode.BROWSE)
        assertSame(browseLoading, RetainedProjection.hydrate(browseLoading, raw))
        assertEquals(listOf("c"), RetainedProjection.hydrate(ViewState(), raw).nodes.map { it.id })
    }

    @Test
    fun `conflict token is allowlisted and mapped to a readable error state`() {
        assertEquals("onboarding:ONBOARDING_INTENT_CONFLICT",
            NativeStderrCodes.code("onboarding: ONBOARDING_INTENT_CONFLICT"))
        assertTrue(NativeStderrCodes.ONBOARDING_TOKENS.contains("ONBOARDING_INTENT_CONFLICT"))
        val text = UserStatusText.error("ONBOARDING_INTENT_CONFLICT").orEmpty()
        assertTrue(text.startsWith("ONBOARDING_INTENT_CONFLICT\n"))
        assertFalse(text.contains("HOST_ERROR"))
        assertTrue(text.contains("Завершите текущую попытку"))

        val mirror = service.substringAfter("private fun mirrorNativeStderr(")
            .substringBefore("private fun sign(")
        assertTrue(mirror.contains("\"onboarding:ONBOARDING_INTENT_CONFLICT\""))
        assertTrue(mirror.contains("view.copy(error = \"ONBOARDING_INTENT_CONFLICT\")"))
        assertTrue(service.contains("{ code -> mirrorNativeStderr(attempt, code) }"))
    }

    @Test
    fun `offline browse failure becomes an explicit retry state not an empty list`() {
        assertEquals("TRANSPORT", BrowseErrorPolicy.fromStderr("TRANSPORT", waitingState()))
        assertEquals("SERVICE_UNAVAILABLE", BrowseErrorPolicy.fromStderr("SERVICE_UNAVAILABLE", waitingState()))
        assertNull(BrowseErrorPolicy.fromStderr("SESSION_EXPIRED", waitingState()))
        assertNull(BrowseErrorPolicy.fromStderr("TRANSPORT",
            ViewState(phase = "CatalogReady", nodes = listOf(NodeLabel("v", "V", "FI")))))
        // A browse display never falls back to stale verified content: the offline state wins.
        assertEquals("TRANSPORT", BrowseErrorPolicy.fromStderr("TRANSPORT",
            waitingState().copy(displayMode = CatalogDisplayMode.BROWSE,
                nodes = listOf(NodeLabel("v", "V", "FI")))))
        val mirror = service.substringAfter("private fun mirrorNativeStderr(")
            .substringBefore("private fun sign(")
        assertTrue(mirror.contains("view.copy(browseError = it)"))
        assertTrue(catalogView.contains("retry: Boolean = false"))
        assertTrue(catalogView.contains("text = \"Попробовать ещё раз\""))
        // A successful empty answer is rendered as its own state, never as an error card.
        val browse = catalogView.substringAfter("private fun addBrowse(").substringBefore("private fun addBrowseRow(")
        assertTrue(browse.contains("if (error == null) addEmpty()"))
        assertFalse(browse.contains("addLoading()"))
    }

    @Test
    fun `auto-load claims once per process and ignores the cached list`() {
        assertTrue(ProcessAutoLoad.claim())
        assertFalse(ProcessAutoLoad.claim())
        assertTrue(AutoLoadPolicy.shouldStart(emptyList()))
        assertTrue(AutoLoadPolicy.shouldStart(listOf(NodeLabel("v", "V", "DE"))))
        val auto = activity.substringAfter("private fun autoLoadSavedSubscription()")
            .substringBefore("\n    }")
        assertTrue(auto.contains("AutoLoadPolicy.shouldStart(projectedState().nodes)"))
        assertTrue(auto.contains("ProcessAutoLoad.claim()"))
        assertTrue(auto.contains("setAction(\"resume\")"))
        assertFalse(auto.contains("isNotEmpty()"))
        assertTrue(activity.substringAfter("override fun onCreate(").contains("autoLoadSavedSubscription()"))
        // Every initial screen uses normal mobile resume; external data is never imported.
        assertFalse(activity.contains("incomingImport"))
        assertFalse(activity.contains("handleIncomingIntent"))
    }

    @Test
    fun `browse display branch outranks stale summary and is mode-gated in the catalog view`() {
        val render = catalogView.substringAfter("fun render(state: ViewState")
            .substringBefore("private fun addAppBar")
        val browseAt = render.indexOf("browseMode -> addBrowse(")
        val expiredAt = render.indexOf("expired -> addExpired(")
        assertTrue("browse display must precede the stale expired card", browseAt in 1 until expiredAt)
        assertTrue(render.contains("val browseMode = BrowseCatalogCodec.isDisplayed(state)"))
        assertFalse(render.contains("state.nodes.isEmpty() && (state.browseLoaded"))
        assertTrue(catalogView.contains("BrowseCatalogCodec.isDisplayed(state)"))

        // An unlinked mobile attempt starts in browse mode; a credential answer switches it back.
        assertTrue(service.contains("displayMode = if (mobileBaseUrl != null && activeLink.isEmpty()) CatalogDisplayMode.BROWSE"))
        assertTrue(service.contains("RetainedCatalogPolicy.connectableId(view) == id"))
    }

    @Test
    fun `credential catalog and connect paths stay on the existing branch`() {
        assertTrue(service.contains("NodeSelection.parseCatalog(event)"))
        assertTrue(service.contains("NodeSelection.applyCatalog(view, catalog, summary)"))
        assertTrue(service.contains("runCatching { persistCatalogCache(updated) }"))
        assertTrue(service.contains("autoConnectRetained(attempt, updated)"))
        assertTrue(service.contains("RetainedCatalogPolicy.connectableId("))
        assertTrue(service.contains("send(JSONObject().put(\"type\", \"select_node\").put(\"node_id\", target)"))
        assertEquals(1, Regex("JSONObject\\(\\)\\.put\\(\"type\", \"explicit_connect\"\\)").findAll(service).count())
        assertTrue(service.contains("mobileCatalog.onCatalogAccepted(attempt)"))
        assertEquals(3, Regex("catalogTimer\\.apply\\(").findAll(service).count())
        assertTrue(catalogView.contains("CatalogRenderPolicy.showRetained(state.phase, state.nodes)"))
        assertTrue(catalogView.contains("addContent(state, readOnly = true)"))
    }
}

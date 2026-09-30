package xyz.terlimo.test

import android.app.*
import android.content.BroadcastReceiver
import android.content.Context
import android.content.IntentFilter
import android.content.Intent
import android.net.*
import android.view.ViewConfiguration
import android.os.*
import com.wireguard.android.backend.Tunnel
import com.wireguard.config.Config
import com.wireguard.config.InetNetwork
import org.json.JSONObject
import java.io.ByteArrayInputStream
import java.net.URL
import java.security.KeyFactory
import java.security.interfaces.ECPublicKey
import java.security.spec.X509EncodedKeySpec
import java.time.Instant
import java.util.UUID
import java.util.concurrent.CopyOnWriteArraySet
import java.util.concurrent.Executors
import java.util.concurrent.ThreadPoolExecutor
import java.util.concurrent.ArrayBlockingQueue
import java.util.concurrent.TimeUnit

internal data class ViewState(val phase: String = "Idle", val attempt: String? = null,
    val nodes: List<NodeLabel> = emptyList(), val error: String? = null,
    val summary: CatalogSummary? = null, val selectedNodeId: String = "", val pendingNodeId: String? = null,
    val pings: Map<String, NodePingState> = emptyMap(), val pingAll: PingAllState = PingAllState(),
    val traffic: TrafficSnapshot = TrafficSnapshot(),
    val serverUsage: ServerUsage? = null, val serverUsageUnavailable: Boolean = false, val catalogRevision: String = "",
    val pendingSwitchId: String? = null, val pendingSwitchRevision: String? = null,
    val wakeRecovery: WakeRecoveryStatus? = null, val channels: ChannelsStatus? = null,
    val accountAccess: AccountAccessSnapshot? = null,
    // Display-only browse list of the owner-contract catalog before access plus its host-local
    // preference. Never an admission snapshot; never the verified nodes/selection above. The
    // display mode separates that fresh browse display from the retained verified credentials.
    val browseNodes: List<NodeLabel> = emptyList(), val browseLoaded: Boolean = false,
    val browseSelectedId: String = "", val browseError: String? = null,
    val displayMode: String = CatalogDisplayMode.CREDENTIAL,
    // S3-A Telegram registration display state (server-owned; never eligibility truth).
    val registration: RegistrationState? = null,
    // S3-B trial display state (server-owned; activation action only on can_activate).
    val trial: TrialState? = null,
    // S5 purchase/renewal display state (server-owned results only; never a grant and
    // never an entitlement write: confirmation follows the fresh /me projection alone).
    val purchase: PurchaseState? = null,
    // S5 §11 one-way announcements display state (server-owned; host owns read-marker keys).
    val announcements: AnnouncementsUi = AnnouncementsUi(),
    // §§18–19 connected-devices display state (server-owned; read/delete only).
    val devices: DevicesUi? = null)
internal data class CaptchaPrompt(val attempt: String, val id: String, val url: String, val deadlineElapsed: Long, val complete: (String) -> Unit)

class SessionService : Service() {
    private lateinit var retention: ConnectionRetentionController
    private var sleepPaused = false
    private var sleepResumeRequested = false
    // Node to connect to once the freshly verified catalog arrives (one-tap Connect
    // from the retained disconnected state). Cleared on stop/failure; only the
    // active attempt may consume it.
    private var connectOnCatalog: String? = null
    // Explicit gateway choice while a protected hold (KillSwitch) is active. The tapped target
    // is applied only after the previous native child stop is confirmed, a fresh verified
    // catalog is received and native admission succeeds; never from cache.
    private val holdLock = Any()
    @Volatile private var holdFailover = HoldFailoverState()
    @Volatile private var holdStopGeneration = 0L
    /** Cancels the pending choice and invalidates any outstanding stop completion callback. */
    private fun invalidateHoldFailover() {
        holdStopGeneration++
        clearHoldFailover()
    }
    private fun clearHoldFailover() {
        synchronized(holdLock) { holdFailover = HoldFailoverPolicy.cleared() }
    }
    private var lifecycleRegistered = false
    private val lifecycleRevision = java.util.concurrent.atomic.AtomicLong(0)
    @Volatile private var activeRuntimeEpoch = 0L
    // Monotonic acceptance time of the last forwarded manual refresh intent (double-tap guard).
    private var lastRefreshIntentElapsed: Long? = null
    @Volatile private var rollbackRuntimeEpoch = 0L
    private val lifecycleReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            if (intent?.action != Intent.ACTION_SCREEN_ON && intent?.action != Intent.ACTION_SCREEN_OFF) return
            // Elapsed lease time is checked before any wake-triggered recovery.
            expiry.run()
            retention.screenChanged()
            val attempt = gate.active ?: return
            val sleeping = !getSystemService(PowerManager::class.java).isInteractive
            val revision = lifecycleRevision.incrementAndGet()
            publishActive(attempt, view.copy(wakeRecovery = null, channels = null))
            submitControl {
                if (gate.active == attempt && !stopping.get())
                    send(JSONObject().put("type", if (sleeping) "device_sleep" else "device_wake")
                        .put("lifecycle_revision", revision))
            }
        }
    }
    private val statusListener: (ViewState) -> Unit = { state ->
        main.post { if (!stopping.get() && view === state) {
            retention.update(state.phase == "Connected", sleepPaused, physical)
            updateForegroundState(state)
        } }
    }
    private val main = Handler(Looper.getMainLooper())
    private val actor = BridgeActor()
    private var foregroundPromoted = false
    private var scheduleListener: ((CatalogRefreshCoordinator.ScheduleChange) -> Unit)? = null
    private val catalogGate = CatalogRefreshScheduleState.gate
    private val catalogRefreshObservers = CatalogRefreshRequestRegistry()
    private val vpnWorker = Executors.newSingleThreadExecutor()
    private val signer = ThreadPoolExecutor(1, 1, 0, TimeUnit.MILLISECONDS, ArrayBlockingQueue(4))
    private val gate = AttemptGate()
    private val connectDeadline = ConnectDeadline()
    // S5 07.2 probe attribution: one bounded id per manual request (ping-all reuses the
    // same factories through a ProbeRunPlan) and one active fence per request.
    private val probeIds = ProbeIds()
    private val probeFrames = ProbeFrameHandler()
    // Explicit-Connect gate for the pre-admission onboarding-hour intent: armed only
    // by the true user Connect paths (select, one-tap retained, pre-admission first
    // connect), cleared on stop. No behavior change.
    private val explicitConnect = ExplicitConnectArming()
    // Pending pre-admission first connect: set only by the explicit after-consent
    // action, consumed by the next child start, never set by background/resume/import.
    // The optional selected browse gateway travels with it as the explicit gateway_key.
    @Volatile private var preAdmissionConnect = false
    @Volatile private var preAdmissionGatewayKey = ""
    private val stopping = java.util.concurrent.atomic.AtomicBoolean(false)
    private val vpnApplying = java.util.concurrent.atomic.AtomicBoolean(false)
    private lateinit var storage: InstallationStore
    private lateinit var connectivity: ConnectivityManager
    private var backend: SafeGoBackend? = null
    @Volatile private var native: NativeProcess? = null
    @Volatile private var physical: Network? = null
    private val physicalAvailability = PhysicalAvailability<Network>()
    @Volatile private var readinessProbe: VpnReadinessProbe? = null
    /** Explicit per-attempt catalogue expectation state machine (mobile rights transitions). */
    private val mobileCatalog = MobileCatalogGate()
    /** S3-B explicit trial activation action gate (service-only path, no VPN required). */
    private val trialGate = TrialActivateGate()
    /** S5 explicit purchase action gate (cold service-only attempt when none is active). */
    private val purchaseGate = PurchaseGate()
    /** §§18–19 devices list/delete cold service-only gate (same bounded flow as purchase). */
    private val devicesGate = DevicesColdGate()
    /** Explicit "already have account -> Telegram sign in" gate (existing registration flow). */
    private val loginGate = RegistrationLoginGate()
    /** Durable host-owned idempotency keys for the purchase attempt; loaded lazily after storage. */
    private val purchaseAttempts by lazy { PurchaseAttempts(InstallationPurchaseAttemptStore(storage)) }
    /** Local correlation of the explicitly sent payment_create with its result (no wire pairing). */
    private val paymentCreates = PaymentCreateTracker()
    /** Host-owned single-flight: at most one Quote/Payment request outstanding on the stream. */
    private val purchaseFlight = PurchaseSingleFlight()
    /** Truthful send orchestration around the single-flight capture (no swallowed exceptions). */
    private val purchaseSender = PurchaseSender(purchaseFlight)
    /** Finite catalogue window timer; applies the bounded gate actions to the existing lifecycle. */
    private val catalogTimer = CatalogDeadlineTimer(
        schedule = { delayMillis, callback -> main.postDelayed(callback, delayMillis) },
        activeAttempt = { gate.active },
        onTimeout = { attempt -> handleAttemptTerminal(attempt, "CATALOG_TIMEOUT") },
        elapsed = { android.os.SystemClock.elapsedRealtime() },
        utc = { System.currentTimeMillis() },
        emit = { marker -> logCatalogMarker(marker) },
    )

    /**
     * Secret-safe catalogue timing marker: fixed kind, attempt prefix, monotonic elapsed ms
     * since ARM, UTC ms, and the fixed rights/accepted-revision detail. Never carries account,
     * keys, URLs or catalogue contents; it never influences the timer decision.
     */
    private fun logCatalogMarker(marker: CatalogTimerMarkerRecord) {
        android.util.Log.w("WDTT/Catalog", CatalogMarkerSanitizer.line(marker))
    }
    @Volatile private var activeVpnConfig: Config? = null
    @Volatile private var activeRoutingPlan: RoutingVpnPlan? = null
    @Volatile private var activeVpnNodeId: String = ""
    @Volatile private var rollbackVpnConfig: Config? = null
    @Volatile private var rollbackRoutingPlan: RoutingVpnPlan? = null
    @Volatile private var rollbackVpnNodeId: String = ""
    @Volatile private var rollbackBackend: SafeGoBackend? = null
    @Volatile private var rollbackExpiresElapsed: Long = 0
    @Volatile private var rollbackLeaseEnd: String = ""
    private var networkCallback: ConnectivityManager.NetworkCallback? = null
    @Volatile private var networkRecovery: NetworkRecoveryState? = null
    @Volatile private var recoveryNetwork: Network? = null
    @Volatile private var recoveryScheduled = false
    @Volatile private var scheduledRecovery: Runnable? = null
    @Volatile private var recoveryNativeStopped = true
    @Volatile private var recoveryConnectAttempt: String? = null
    @Volatile private var networkGeneration = 0L
    private var activeLink = ""
    private lateinit var probeSettings: NodeProbeSettings
    @Volatile private var expiresElapsed = 0L
    private var leaseEnd = ""
    private lateinit var leaseAlarms: RetentionAlarmScheduler
    private val expiry = Runnable {
        if (!stopping.get() && LeaseExpiryPolicy.due(expiresElapsed, SystemClock.elapsedRealtime())) {
            if (sleepPaused) {
                // No native transport exists while sleeping. Keep the blocked TUN;
                // waking must refresh/admit a new grant before any traffic resumes.
                publish(view.copy(error = "LEASE_EXPIRED"))
            } else if (activeVpnConfig != null || networkRecovery != null || view.phase == "KillSwitch") {
                // Expiry never revives a pending held choice.
                invalidateHoldFailover()
                holdKillSwitch("LEASE_EXPIRED")
            } else stopAttempt("LEASE_EXPIRED")
        }
    }
    private val recoveryExpiry = Runnable {
        val current = networkRecovery ?: return@Runnable
        // The 30 s window bounds one series, not the recovery duty. An attempt still pending
        // decides on its own terminal; a usable path present at the deadline means the server
        // is unreachable (hold); with no usable physical path the protected TUN stays and a
        // later network callback resumes the same node.
        if (gate.active != null || current.inFlight || recoveryScheduled || current.awaitingNetwork) return@Runnable
        val network = recoveryNetwork ?: physical
        if (network != null && networkCandidate(network).usable) holdKillSwitch("PHYSICAL_NETWORK_UNAVAILABLE")
        else awaitRecoveryNetwork(current)
    }
    /**
     * Main-thread display-only countdown refresh for the foreground notification. It
     * rewrites only the notification text from the accepted snapshot; it never republishes
     * the view, never talks to native, never stops and never extends anything. Cancelled
     * with the attempt and with the Service.
     */
    private val accessTick = object : Runnable {
        override fun run() {
            if (gate.active == null || stopping.get()) return
            val snapshot = view.accountAccess ?: return
            if (snapshot.projection.grant.dataAccess != "onboarding_hour") return
            updateForegroundState(view)
            if (AccountAccessDisplayRefresh.shouldPost(
                    gate.active != null && !stopping.get(), snapshot, SystemClock.elapsedRealtime()))
                main.postDelayed(this, AccountAccessDisplayRefresh.TICK_MILLIS)
        }
    }
    private val trafficSampler = TrafficSampler()
    private val trafficReadGate = TrafficReadGate()
    private val TRAFFIC_TICK_MILLIS = 1_000L
    private val TRAFFIC_NOTIFY_MILLIS = 5_000L
    private var lastNotificationBase = ""
    private var lastTrafficNotifyMs = 0L
    private val trafficTick = object : Runnable {
        override fun run() {
            if (stopping.get() || view.phase != "Connected") return
            val attempt = gate.active
            val epoch = activeVpnNodeId
            val wireguard = backend
            if (wireguard != null && attempt != null) {
                // getStatistics is a blocking JNI config read: run it on the existing
                // single-thread service worker, then publish on main under a monotonic
                // generation fence (attempt/node/phase strings alone reset to equal values).
                val capturedGeneration = trafficReadGate.current()
                runCatching {
                    vpnWorker.execute {
                        val stats = runCatching { wireguard.getStatistics(tunnel) }.getOrNull()
                        val peer = stats?.peers()?.singleOrNull()?.let { stats.peer(it) }
                        main.post {
                            if (!trafficReadGate.accepts(capturedGeneration) || stopping.get() ||
                                gate.active != attempt || activeVpnNodeId != epoch ||
                                view.phase != "Connected") return@post
                            if (peer == null) {
                                // Empty/ambiguous sample: never fabricate a measured zero.
                                val honest = view.traffic.copy(measured = false, rxRateBps = 0, txRateBps = 0)
                                if (honest != view.traffic) publish(view.copy(traffic = honest))
                            } else {
                                val snapshot = trafficSampler.onPeerSample(
                                    peer.rxBytes, peer.txBytes, SystemClock.elapsedRealtime(), epoch)
                                if (snapshot != view.traffic) publish(view.copy(traffic = snapshot))
                            }
                        }
                    }
                }
            }
            main.postDelayed(this, TRAFFIC_TICK_MILLIS)
        }
    }
    private val USAGE_TICK_MILLIS = 60_000L
    private val usageTick = object : Runnable {
        override fun run() {
            if (stopping.get() || view.phase != "Connected") return
            val attempt = gate.active ?: return
            requestUsageRead(attempt)
            main.postDelayed(this, USAGE_TICK_MILLIS)
        }
    }
    private fun requestUsageRead(attempt: String) {
        submitControl {
            if (gate.active == attempt && !stopping.get() && view.phase == "Connected") {
                native?.send(JSONObject().put("type", UsageContract.ACTION_USAGE_READ))
            }
        }
    }
    private fun armUsageTick() {
        if (!stopping.get() && view.phase == "Connected") {
            main.removeCallbacks(usageTick)
            main.postDelayed(usageTick, USAGE_TICK_MILLIS)
        }
    }
    private fun stopUsageTick() = main.removeCallbacks(usageTick)

    /**
     * §11 bounded announcements list request. Reuses the existing refresh/idle slots: it is
     * sent after an accepted current /me projection and when the Help «Уведомления» section
     * is opened. No timer, no background poll and nothing before VPN.
     */
    private fun requestAnnouncements() {
        if (stopping.get() || gate.active == null) return
        native?.send(JSONObject().put("type", "announcements_list"))
    }

    /**
     * §11 read marker. The host owns the Idempotency-Key: a retry of the same announcement
     * reuses the exact key bytes and a new key is issued only for a new id. Unread/red dot
     * are cleared solely by a matching read:true acknowledgement.
     */
    private fun openAnnouncement(announcementId: String) {
        if (stopping.get() || announcementId.isEmpty()) return
        val (next, request) = AnnouncementsPolicy.beginRead(view.announcements, announcementId,
            newKey = { "terlimo-ann-" + UUID.randomUUID() })
        if (next != view.announcements) publish(view.copy(announcements = next))
        if (request == null) return
        native?.send(JSONObject().put("type", "announcement_read")
            .put("announcement_id", request.announcementId)
            .put("idempotency_key", request.key))
    }
    private val tunnel = object : Tunnel {
        override fun getName() = "TERLIMO"
        override fun onStateChange(newState: Tunnel.State) {
            if (newState == Tunnel.State.DOWN && gate.active != null && view.phase == "Connected") stopAttempt("VPN_STOPPED")
        }
    }

    override fun onCreate() {
        super.onCreate()
        runningService = this
        retention = ConnectionRetentionController(this, ::pauseForSleep, ::resumeFromSleep)
        leaseAlarms = RetentionAlarmScheduler(this, "lease-${UUID.randomUUID()}") { _, deadline ->
            main.post {
                if (!stopping.get() && deadline == expiresElapsed) expiry.run()
            }
        }
        listeners.add(statusListener)
        val lifecycleFilter = IntentFilter().apply {
            addAction(Intent.ACTION_SCREEN_ON)
            addAction(Intent.ACTION_SCREEN_OFF)
        }
        if (Build.VERSION.SDK_INT >= 33) registerReceiver(lifecycleReceiver, lifecycleFilter, Context.RECEIVER_NOT_EXPORTED)
        else registerReceiver(lifecycleReceiver, lifecycleFilter)
        lifecycleRegistered = true
        storage = InstallationStore(this)
        // Seed the last verified catalog/selection from durable storage when no
        // session is active, so an ordinary Disconnect survives Service recreation.
        if (view.nodes.isEmpty()) {
            runCatching { storage.readCatalogCache() }.getOrNull()
                ?.let { CatalogCacheCodec.decode(it) }
                ?.takeIf { it.nodes.isNotEmpty() }
                ?.let { publish(view.copy(nodes = it.nodes, selectedNodeId = it.selectedNodeId, catalogRevision = it.revision)) }
        }
        connectivity = getSystemService(ConnectivityManager::class.java)
        getSystemService(NotificationManager::class.java).createNotificationChannel(
            NotificationChannel("test-vpn", "TERLIMO VPN", NotificationManager.IMPORTANCE_LOW))
        val listener: (CatalogRefreshCoordinator.ScheduleChange) -> Unit = { change ->
            submitControl { onScheduleChanged(change) }
        }
        scheduleListener = listener
        CatalogRefreshScheduleState.addScheduleListener(listener)
    }

    /**
     * §26.5: a bound-only job instance must not become a foreground service or post the VPN
     * notification. The ordinary started-service paths (UI/startForegroundService, retention
     * alarm) promote exactly as before; the 5s startForeground window is kept because the
     * promotion happens at the top of onStartCommand.
     */
    private fun promoteToForeground() {
        if (foregroundPromoted) return
        foregroundPromoted = true
        val open = PendingIntent.getActivity(this, 0,
            Intent(this, MainActivity::class.java).putExtra(AnnouncementDeepLink.EXTRA_OPEN_ANNOUNCEMENTS, true),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val stop = PendingIntent.getService(this, 1, Intent(this, SessionService::class.java).setAction("cancel"), PendingIntent.FLAG_IMMUTABLE)
        startForeground(1, Notification.Builder(this, "test-vpn").setContentTitle("TERLIMO")
            .setContentText("VPN-подключение").setSmallIcon(android.R.drawable.stat_sys_warning)
            .setContentIntent(open).addAction(Notification.Action.Builder(null, "Отключить", stop).build())
            .setOngoing(true).build())
        updateForegroundState(view)
    }
    private fun updateForegroundState(state: ViewState) {
        // §26.5: a bound-only job instance must never post the VPN notification; the ordinary
        // started path promotes first and then publishes the same state into the notification.
        if (!foregroundPromoted) return
        val open = PendingIntent.getActivity(this, 0,
            Intent(this, MainActivity::class.java).putExtra(AnnouncementDeepLink.EXTRA_OPEN_ANNOUNCEMENTS, true),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val stop = PendingIntent.getService(this, 1, Intent(this, SessionService::class.java).setAction("cancel"), PendingIntent.FLAG_IMMUTABLE)
        val accountUsage = state.serverUsage
        val usageSegment = if (state.phase == "Connected") {
            if (accountUsage != null) ServerUsageText.notificationSegment(accountUsage, SystemClock.elapsedRealtime())
            else if (state.serverUsageUnavailable) "Зачёт: нет данных" else null
        } else null
        val base = listOfNotNull(UserStatusText.phase(state.phase),
            NotificationText.serverLabel(state.nodes.singleOrNull { it.id == state.selectedNodeId }?.name),
            ChannelsDisplay.line(state.phase == "Connected", state.channels, state.wakeRecovery),
            AccountAccessPolicy.notificationLine(state.accountAccess, SystemClock.elapsedRealtime()),
            usageSegment).joinToString(" · ")
        val traffic = state.traffic
        // §9: an active connection always shows its incoming/outgoing traffic, zero included.
        val trafficPart = NotificationText.trafficSegment(state.phase, traffic).orEmpty()
        val now = SystemClock.elapsedRealtime()
        val baseChanged = base != lastNotificationBase
        // Traffic is refreshed at most every 5 s so the notification is not updated per tick.
        val trafficDue = trafficPart.isNotEmpty() && now - lastTrafficNotifyMs >= TRAFFIC_NOTIFY_MILLIS
        if (!baseChanged && !trafficDue) return
        lastNotificationBase = base
        if (trafficPart.isNotEmpty()) lastTrafficNotifyMs = now
        val text = if (trafficPart.isEmpty()) base else "$base · $trafficPart"
        getSystemService(NotificationManager::class.java).notify(1, Notification.Builder(this, "test-vpn")
            .setContentTitle("TERLIMO").setContentText(text).setSmallIcon(android.R.drawable.stat_sys_warning)
            .setContentIntent(open).addAction(Notification.Action.Builder(null, "Отключить", stop).build())
            .setOnlyAlertOnce(true).setOngoing(true).build())
    }
    override fun onBind(intent: Intent?): android.os.IBinder = binder

    /**
     * §26.5 in-process control surface for the periodic catalog job. The job gets a lease on
     * the single writer; it can only ask for the existing bounded refresh and cancel its own
     * request. No HTTP, signing, seed or explicit-connect surface is exposed.
     */
    internal inner class LocalBinder : android.os.Binder() {
        fun requestCatalogRefresh(
            requestId: String,
            epochAtStart: Long,
            observer: CatalogRefreshObserver,
        ): Boolean = this@SessionService.requestCatalogRefresh(requestId, epochAtStart, observer)

        fun cancelCatalogRefresh(requestId: String) =
            this@SessionService.cancelCatalogRefresh(requestId)
    }

    private val binder = LocalBinder()

    private fun requestCatalogRefresh(
        requestId: String,
        epochAtStart: Long,
        observer: CatalogRefreshObserver,
    ): Boolean {
        if (stopping.get() || retiringActors.get() > 0) return false
        if (!catalogGate.registerJob(requestId, epochAtStart)) return false
        if (!catalogRefreshObservers.register(requestId, observer)) {
            catalogGate.cancelJob(requestId)
            return false
        }
        val attempt = gate.active
        if (attempt == null) {
            if (!catalogGate.pendCold(requestId)) {
                catalogGate.cancelJob(requestId)
                completeCatalogRefresh(listOf(CatalogRefreshCoordinator.Completion(requestId, false, "rejected")))
                return true
            }
            // The queued begin re-validates the claim on the actor: a cancel/Off that arrived
            // first makes this a rejection instead of a misclassified manual cycle.
            submitCatalogControl(requestId) {
                if (!catalogGate.isColdJobValid(requestId)) {
                    completeCatalogRefresh(
                        listOf(CatalogRefreshCoordinator.Completion(requestId, false, "rejected")))
                } else {
                    begin("", catalogRequestId = requestId)
                }
            }
            return true
        }
        submitCatalogControl(requestId) {
            val live = gate.active
            val child = native
            if (live != attempt || child == null || stopping.get()) {
                catalogGate.cancelJob(requestId)
                completeCatalogRefresh(
                    listOf(CatalogRefreshCoordinator.Completion(requestId, false, "rejected")))
                return@submitCatalogControl
            }
            if (!catalogGate.dispatch(attempt, requestId) {
                    child.trySend(JSONObject().put("type", "refresh_manual"))
                }) {
                catalogGate.cancelJob(requestId)
                completeCatalogRefresh(
                    listOf(CatalogRefreshCoordinator.Completion(requestId, false, "rejected")))
            }
        }
        return true
    }

    /** A periodic request must not vanish when the actor is full or already closing. */
    private fun submitCatalogControl(requestId: String, action: () -> Unit) {
        fun reject() {
            catalogGate.cancelJob(requestId)
            completeCatalogRefresh(listOf(CatalogRefreshCoordinator.Completion(requestId, false, "rejected")))
        }
        val result = actor.submit { if (stopping.get()) reject() else action() }
        if (result != BridgeActor.Result.ACCEPTED) reject()
    }

    private val catalogCancellation by lazy {
        CatalogRefreshCancellation(catalogGate, catalogRefreshObservers,
            { cleanup -> submitControl(cleanup) }, ::stopJobOnlyCycles)
    }

    private fun cancelCatalogRefresh(requestId: String) = catalogCancellation.cancel(requestId)

    /** Stops only the still-active job-only attempts; a manual/VPN takeover is never stopped. */
    private fun stopJobOnlyCycles(attempts: List<String>) {
        attempts.forEach { id ->
            if (gate.active == id && catalogGate.isJobOnly(id)) stopAttempt(null, "schedule_cancelled")
        }
    }

    /** The only terminal delivery path: the registry guarantees exactly one callback. */
    private fun completeCatalogRefresh(completions: List<CatalogRefreshCoordinator.Completion>) {
        if (completions.isEmpty()) return
        main.post {
            completions.forEach { completion ->
                catalogRefreshObservers.deliver(completion.requestId, completion.ok, completion.error)
            }
        }
    }

    /** Off / mode change already applied linearly; execute only the actor-side consequences. */
    private fun onScheduleChanged(change: CatalogRefreshCoordinator.ScheduleChange) {
        if (stopping.get()) return
        completeCatalogRefresh(change.jobFailures)
        stopJobOnlyCycles(change.attemptsToStop)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        promoteToForeground()
        // §26.5: any started-service intent is a live (manual/UI) owner of the cycle. This is
        // queued on the actor before the begin it may trigger, so the ownership decision can
        // never see it out of order. The job only ever binds, so it can never mark its own
        // cycle as manually owned.
        submitControl {
            catalogGate.markServiceIntent()
            catalogGate.markAttemptManual(gate.active)
        }
        if (intent?.action != "cancel" && retiringActors.get() > 0) {
            if (intent?.action == "switch") SwitchDiagnostics.log("service_reject", "retiring")
            return START_NOT_STICKY
        }
        when (intent?.action) {
            "import" -> {
                val link = intent.getStringExtra("link").orEmpty()
                if (gate.active == null && !stopping.get()) submitControl { begin(link) }
            }
            "resume" -> if (gate.active == null && !stopping.get()) submitControl { begin("") }
            "refresh" -> {
                // S5 §11/§29 single manual refresh path with a host double-tap guard: a
                // duplicate intent inside the platform double-tap window is a repeat of the
                // same user tap and is not forwarded. With an active attempt it asks native
                // for exactly one bounded runner cycle over the existing authenticated
                // operations; idle/cold keeps the standard resume/bootstrap.
                val nowElapsed = SystemClock.elapsedRealtime()
                val window = ViewConfiguration.getDoubleTapTimeout().toLong()
                if (!RefreshIntentGate.accept(lastRefreshIntentElapsed, nowElapsed, window)) {
                    android.util.Log.i("WDTT/Refresh", "refresh intent suppressed")
                } else if (!stopping.get()) {
                    lastRefreshIntentElapsed = nowElapsed
                    android.util.Log.i("WDTT/Refresh", "refresh intent accepted")
                    if (gate.active != null) {
                        native?.send(JSONObject().put("type", "refresh_manual"))
                    } else {
                        submitControl { begin("") }
                    }
                }
            }
            "connect" -> {
                val id = intent.getStringExtra("node_id").orEmpty()
                PowerDiagnostics.line("svc.connect.enter",
                    "gateIdle" to (gate.active == null).toString(),
                    "retainedOk" to (RetainedCatalogPolicy.connectableId(view) == id).toString(),
                    "phase" to view.phase,
                    "native" to (native != null).toString(),
                    "stopping" to stopping.get().toString())
                if (gate.active == null && !stopping.get() && RetainedCatalogPolicy.connectableId(view) == id) {
                    PowerDiagnostics.line("svc.connect.begin", "phase" to view.phase)
                    // Start a fresh native attempt; the retained id is applied only
                    // after the new catalog is verified (never from cache directly).
                    connectOnCatalog = id
                    submitControl { begin("") }
                }
            }
            "choose" -> {
                val id = intent.getStringExtra("node_id").orEmpty()
                if (!stopping.get() && view.phase == "CatalogReady" && view.nodes.any { it.id == id }) {
                    publish(view.copy(pendingNodeId = id))
                    submitControl {
                        if (view.phase == "CatalogReady" && view.nodes.any { it.id == id })
                            send(JSONObject().put("type", "choose_node").put("node_id", id))
                    }
                }
            }
            "browse_select" -> {
                // Host-local preference only (owner contract 5): it selects a displayed
                // metadata row, never pings/probes and never talks to native.
                val id = intent.getStringExtra("node_id").orEmpty()
                if (!stopping.get() && BrowseCatalogCodec.displayedNodes(view).any { it.id == id }) {
                    publish(view.copy(browseSelectedId = id))
                }
            }
            "announcements_request" -> requestAnnouncements()
            "devices_refresh" -> {
                // Read-only refresh, offered only after a confirmed Telegram proof. Without a
                // live attempt the same explicit tap starts one bounded service-only attempt
                // (the accepted cold flow) and the single list read is sent exactly once after
                // the first accepted /me on it.
                val liveAttempt = gate.active?.takeIf { native != null }
                val canManage = DevicesPolicy.canManage(view.accountAccess?.projection?.registration)
                val proofUnknown = view.accountAccess == null
                val requestId = "devlist-" + java.util.UUID.randomUUID()
                when (devicesGate.onTap(liveAttempt, canManage || proofUnknown, DevicesRequest.List(requestId))) {
                    DevicesTapAction.START_SERVICE -> {
                        startDevicesColdAttempt()
                    }
                    DevicesTapAction.SEND_NOW -> if (liveAttempt != null) submitControl {
                        if (gate.active == liveAttempt && !stopping.get())
                            requestDevicesList(liveAttempt, requestId)
                        else devicesGate.onResult(liveAttempt)
                    }
                    DevicesTapAction.ERROR -> publish(view.copy(devices = (view.devices ?: DevicesUi())
                        .copy(error = "DEVICE_MANAGEMENT_FORBIDDEN")))
                    DevicesTapAction.IGNORE -> Unit
                }
            }
            "device_delete" -> {
                // Exactly one explicitly chosen device; the host owns both the Idempotency-Key
                // and the correlation id and never repeats the DELETE blindly. Single-flight is
                // enforced in the gate and on the serial service path, not only by a disabled
                // button. Without a live attempt the same bounded cold attempt is used.
                val liveAttempt = gate.active?.takeIf { native != null }
                val deviceId = intent.getStringExtra("device_id").orEmpty()
                val canManage = DevicesPolicy.canManage(view.accountAccess?.projection?.registration)
                val request = if (!stopping.get() && deviceId.isNotEmpty()) DevicesRequest.Delete(
                    requestId = "devdel-" + java.util.UUID.randomUUID(),
                    deviceId = deviceId,
                    idempotencyKey = "terlimo-device-" + java.util.UUID.randomUUID(),
                ) else null
                if (request != null) when (devicesGate.onTap(liveAttempt, canManage, request)) {
                    DevicesTapAction.START_SERVICE -> {
                        startDevicesColdAttempt()
                    }
                    DevicesTapAction.SEND_NOW -> if (liveAttempt != null) submitControl {
                        if (gate.active == liveAttempt && !stopping.get()) performDeviceDelete(liveAttempt, request)
                        else devicesGate.onResult(liveAttempt)
                    }
                    DevicesTapAction.ERROR -> publish(view.copy(devices = (view.devices ?: DevicesUi())
                        .copy(error = "DEVICE_MANAGEMENT_FORBIDDEN")))
                    DevicesTapAction.IGNORE -> Unit
                }
            }
            "announcement_read" -> openAnnouncement(intent.getStringExtra("announcement_id").orEmpty())
            "onboarding_connect" -> {
                // Pre-admission first connect: the separate explicit action issued by the
                // UI only after the mandatory VPN consent. An already running attempt
                // receives the command immediately; a cold start begins one attempt and
                // sends the command right after the child starts. The optional selected
                // browse gateway_key travels with the explicit connect unchanged.
                // While the current display is browse without a current selection there is
                // no legitimate keyless connect (owner contract 5): the intent is refused
                // instead of silently falling back to "no gateway_key".
                val gatewayKey = intent.getStringExtra("gateway_key").orEmpty()
                if (BrowseConnectGate.requiresSelection(view)) {
                    android.util.Log.i("WDTT/Explicit", "stage=refused_browse_selection")
                } else {
                    val attempt = gate.active
                    if (!stopping.get() && attempt != null) {
                        if (explicitConnect.arm(ExplicitConnectGate.Entry.PRE_ADMISSION, attempt)) {
                            native?.send(explicitConnectCommand(gatewayKey))
                            android.util.Log.i("WDTT/Explicit", "stage=sent")
                        }
                    } else if (!stopping.get() && retiringActors.get() == 0 && gate.active == null) {
                        preAdmissionConnect = true
                        preAdmissionGatewayKey = gatewayKey
                        android.util.Log.i("WDTT/Explicit", "stage=deferred")
                        submitControl { begin("") }
                    }
                }
            }
            "select" -> {
                val id = intent.getStringExtra("node_id").orEmpty()
                val attempt = gate.active
                PowerDiagnostics.line("svc.select.enter",
                    "phase" to view.phase,
                    "selValid" to (id == NodeSelection.connectableNodeId(view.nodes, view.selectedNodeId)).toString(),
                    "pending" to (view.pendingNodeId != null).toString(),
                    "attemptLive" to (attempt != null).toString(),
                    "native" to (native != null).toString(),
                    "stopping" to stopping.get().toString())
                if (!stopping.get() && view.phase == "CatalogReady" && view.pendingNodeId == null &&
                    id == NodeSelection.connectableNodeId(view.nodes, view.selectedNodeId) &&
                    VpnService.prepare(this) == null && attempt != null &&
                    connectDeadline.start(attempt, SystemClock.elapsedRealtime())) {
                    PowerDiagnostics.line("svc.select.accepted", "phase" to view.phase)
                    explicitConnect.arm(ExplicitConnectGate.Entry.SELECT, attempt)
                    retention.loadForConnection()
                    retention.transitionFor(ConnectDeadline.MILLIS)
                    main.postDelayed({
                        gate.ifActive(attempt) {
                            if (connectDeadline.expired(attempt, SystemClock.elapsedRealtime()))
                                handleAttemptTerminal(attempt, "VPN_SETUP_TIMEOUT")
                        }
                    }, ConnectDeadline.MILLIS)
                    submitControl {
                        if (view.phase == "CatalogReady" && view.pendingNodeId == null &&
                            id == NodeSelection.connectableNodeId(view.nodes, view.selectedNodeId))
                            send(JSONObject().put("type", "select_node").put("node_id", id)
                                .put("explicit_connect", true))
                    }
                }
            }
            "switch" -> {
                // One fixed diagnostic line per boundary; no raw node/switch/revision values.
                val id = intent.getStringExtra("node_id").orEmpty()
                SwitchDiagnostics.log("service_received")
                if (stopping.get()) {
                    SwitchDiagnostics.log("service_reject", "stopping")
                } else when (val admission = ActiveNodeSwitch.admission(view, id)) {
                    SwitchAdmission.ALLOWED -> {
                        retention.transitionFor(10_000)
                        rollbackRuntimeEpoch = activeRuntimeEpoch
                        activeRuntimeEpoch = 0
                        val switchId = UUID.randomUUID().toString()
                        val revision = view.catalogRevision
                        publish(view.copy(wakeRecovery = null, channels = null, pendingNodeId = id, pendingSwitchId = switchId,
                            pendingSwitchRevision = revision, error = null))
                        SwitchDiagnostics.log("queued")
                        submitControl {
                            SwitchDiagnostics.log("action_entered")
                            if (view.phase == "Connected" && view.pendingNodeId == id &&
                                view.pendingSwitchId == switchId && view.pendingSwitchRevision == revision &&
                                view.nodes.any { it.id == id }) {
                                val result = sendSwitchNode(JSONObject().put("type", "switch_node").put("node_id", id)
                                    .put("switch_id", switchId).put("catalog_revision", revision))
                                // `ok` only when the host actually wrote+flushed to the child stdin;
                                // this is not proof that the runner consumed it.
                                SwitchDiagnostics.log("host_send", result)
                            } else SwitchDiagnostics.log("action_reject", "recheck")
                        }
                    }
                    else -> SwitchDiagnostics.log("service_reject", SwitchDiagnostics.admissionReason(admission))
                }
            }
            "hold_select" -> {
                // UX §5: another gateway stays selectable while the protection is held.
                // The fresh attempt starts only after the previous child stop is confirmed;
                // until then only the explicit target is armed.
                val id = intent.getStringExtra("node_id").orEmpty()
                val now = SystemClock.elapsedRealtime()
                val (next, decision) = synchronized(holdLock) {
                    val result = HoldFailoverPolicy.tap(holdFailover, id, view.phase,
                        activeVpnConfig != null, view.nodes.any { it.id == id },
                        !LeaseExpiryPolicy.due(expiresElapsed, now), recoveryNativeStopped, holdStopGeneration)
                    holdFailover = result.first
                    result
                }
                when (decision) {
                    is HoldFailoverStart.Start -> startHeldFailover(decision.target)
                    is HoldFailoverStart.Wait -> Unit // completion resumes the armed choice
                    HoldFailoverStart.Rejected, HoldFailoverStart.Ignored -> Unit
                }
            }
            "probe" -> {
                val id = intent.getStringExtra("node_id").orEmpty()
                val running = view.pings[id] as? NodePingState.Running
                val tap = if (stopping.get()) ProbeGate.Tap.IGNORE else ProbeGate.single(
                    phase = view.phase,
                    nodeKnown = view.nodes.any { it.id == id },
                    targetRunning = running != null,
                    pingAllActive = view.pingAll.active,
                    anyRunning = view.pings.values.any { it is NodePingState.Running },
                )
                when (tap) {
                    ProbeGate.Tap.CANCEL -> {
                        // Retire the attributed request first: a queued result of this run
                        // can never settle a later request for the same node.
                        probeFrames.cancel(id)
                        native?.send(JSONObject().put("type", "cancel_probe"))
                        publish(view.copy(pings = view.pings + (id to NodePingState.Cancelled)))
                    }
                    ProbeGate.Tap.START -> {
                        val attempt = gate.active
                        if (attempt != null) {
                            val now = SystemClock.elapsedRealtime()
                            val probeId = probeIds.nextManual()
                            probeFrames.begin(ProbeFence(id, probeId, attempt, activeRuntimeEpoch))
                            publish(view.copy(pings = view.pings + (id to NodePingState.Running(now, now, now + 12_000, probeId))))
                            native?.send(JSONObject().put("type", "probe_node").put("node_id", id).put("probe_id", probeId))
                        }
                    }
                    ProbeGate.Tap.IGNORE -> Unit
                }
            }
            "probe_all" -> {
                val probeable = view.nodes.map { it.id }.filter { probeSettings[it] != null }
                val anyRunning = view.pings.values.any { it is NodePingState.Running }
                val decision = if (stopping.get()) ProbeGate.AllTap.IGNORE else ProbeGate.all(
                    phase = view.phase,
                    pingAllActive = view.pingAll.active,
                    anyRunning = anyRunning,
                    probeable = probeable.size,
                )
                // Manual all-node probing runs both pre-connect and while Connected: the
                // native branch probes the current node on the live session and every other
                // node on its own fenced direct echo connection, without switching.
                if (decision == ProbeGate.AllTap.START) {
                    val step = PingAllGate.start(view.pingAll, probeable)
                    val first = step.startId
                    val attempt = gate.active
                    if (first != null && attempt != null) {
                        val now = SystemClock.elapsedRealtime()
                        val probeId = probeIds.nextManual()
                        probeFrames.begin(ProbeFence(first, probeId, attempt, activeRuntimeEpoch))
                        publish(view.copy(pings = view.pings + (first to NodePingState.Running(now, now, now + 12_000, probeId)),
                            pingAll = step.state))
                        native?.send(JSONObject().put("type", "probe_node").put("node_id", first).put("probe_id", probeId))
                    }
                }
            }
            "probe_all_cancel" -> {
                if (view.pingAll.active) {
                    view.pingAll.expectedId?.let { probeFrames.cancel(it) }
                    native?.send(JSONObject().put("type", "cancel_probe"))
                    publish(view.copy(pings = PingAllGate.cancelPings(view.pings),
                        pingAll = PingAllGate.cancel(view.pingAll)))
                }
            }
            "telegram_register" -> {
                val attempt = gate.active
                if (!stopping.get() && attempt != null) native?.send(JSONObject().put("type", "request_telegram_registration"))
            }
            "telegram_login" -> {
                // Existing-account sign-in on a fresh installation: reachable without TUN/hour.
                // A live attempt sends once; otherwise one bounded service-only attempt is
                // started and the single link request is sent after its first accepted /me.
                val attempt = gate.active
                when (loginGate.onTap(attempt, RegistrationUi.loginVisible(view))) {
                    RegistrationLoginTapAction.SEND_NOW -> {
                        val live = attempt
                        if (live != null) submitControl {
                            if (gate.active == live && !stopping.get())
                                native?.send(JSONObject().put("type", "request_telegram_registration"))
                        }
                    }
                    RegistrationLoginTapAction.START_SERVICE -> submitControl { begin("") }
                    RegistrationLoginTapAction.ERROR -> publish(view.copy(registration =
                        (view.registration ?: RegistrationState()).copy(error = "ACCESS_DENIED")))
                    RegistrationLoginTapAction.IGNORE -> Unit
                }
            }
            "telegram_refresh" -> {
                val attempt = gate.active
                if (!stopping.get() && attempt != null) native?.send(JSONObject().put("type", "refresh_telegram_registration"))
            }
            "trial_activate" -> {
                if (!stopping.get()) {
                    when (trialGate.onTap(gate.active, view.accountAccess?.projection?.trial?.canActivate == true)) {
                        TrialTapAction.SEND_NOW -> native?.send(JSONObject().put("type", "activate_trial"))
                        TrialTapAction.START_SERVICE -> submitControl { begin("") }
                        TrialTapAction.ERROR -> publish(view.copy(trial =
                            (view.trial ?: TrialState()).copy(error = "TRIAL_NOT_AVAILABLE")))
                        TrialTapAction.IGNORE -> Unit
                    }
                }
            }
            "purchase_plans" -> handlePurchaseOperation(PurchaseOperation.Plans)
            "purchase_quote" -> {
                val planId = intent.getStringExtra("plan_id").orEmpty()
                val durationCode = intent.getStringExtra("duration_code").orEmpty()
                val method = intent.getStringExtra("method").orEmpty()
                // Only the currently displayed server plan may be quoted; the extras are
                // never trusted past that check.
                val plan = view.purchase?.plans?.firstOrNull { it.planId == planId }
                if (purchaseFlight.busy()) {
                    publish(view.copy(purchase = PurchaseFlow.sending(view.purchase)))
                } else if (!stopping.get() && plan != null && plan.durationCode == durationCode &&
                    method in plan.methods) {
                    val selected = PurchaseFlow.selectMethod(
                        PurchaseFlow.selectPlan(view.purchase, planId), method)
                    publish(view.copy(purchase = selected))
                    handlePurchaseOperation(PurchaseOperation.Quote(planId, durationCode, method))
                } else if (!stopping.get()) {
                    publish(view.copy(purchase = PurchaseFlow.failure(view.purchase, "INVALID_REQUEST")))
                }
            }
            "purchase_pay" -> {
                val quoteId = intent.getStringExtra("quote_id").orEmpty()
                val current = view.purchase
                when {
                    // A create is still outstanding: a second Pay is not queued and the durable
                    // attempt keys are not rotated while the request is unanswered.
                    purchaseFlight.busy() ->
                        publish(view.copy(purchase = PurchaseFlow.sending(view.purchase)))
                    quoteId.isEmpty() || current?.quote?.quoteId != quoteId ||
                        current?.phase == PurchaseFlow.UNAVAILABLE ||
                        current?.selectedPlanId == null ||
                        current?.selectedMethod != current?.quote?.method ||
                        current?.plans?.firstOrNull { it.planId == current.selectedPlanId }
                            ?.durationCode != current?.quote?.durationCode ->
                        publish(view.copy(purchase = PurchaseFlow.failure(current, "QUOTE_EXPIRED")))
                    // An expired quote is a new attempt: keys rotate, the stale quote is dropped
                    // and is never silently reused for a new payment_create. Its create
                    // correlation cannot survive the attempt either.
                    PurchaseFlow.quoteExpired(current, java.time.Instant.now()) -> {
                        purchaseAttempts.restart()
                        paymentCreates.clear()
                        publish(view.copy(purchase = PurchaseFlow.quoteExpiredState(current)))
                    }
                    else -> handlePurchaseOperation(PurchaseOperation.Payment(quoteId))
                }
            }
            "purchase_check" -> {
                val paymentId = view.purchase?.payment?.paymentId
                if (!stopping.get() && paymentId != null) {
                    handlePurchaseOperation(PurchaseOperation.PaymentGet(paymentId))
                }
            }
            "cancel" -> {
                PowerDiagnostics.line("svc.cancel.enter",
                    "phase" to view.phase,
                    "attemptLive" to (gate.active != null).toString(),
                    "native" to (native != null).toString())
                stopAttempt(null, "user_cancel")
            }
        }
        return START_NOT_STICKY
    }
    private fun begin(link: String, requiredNetwork: Network? = null, recoveryGeneration: Long? = null,
        catalogRequestId: String? = null) {
        var startedAttempt: String? = null
        try {
            lastReadiness = emptyMap()
            lastRelay = emptyMap()
            lastVpnDiagnostic = emptyMap()
            lastBootstrap = emptyMap()
            lastPhysicalNetwork = mapOf("network_present" to false, "network_internet" to false,
                "network_not_vpn" to false, "network_validated" to false, "network_default_match" to false)
            if (stopping.get()) return
            check(retiringActors.get() == 0) { "CLEANUP_PENDING" }
            check(gate.active == null) { "ATTEMPT_ACTIVE" }
            val issuers = JSONObject(assets.open("issuers.json").bufferedReader().use { it.readText() })
            check(issuers.length() > 0) { "TRUST_NOT_PROVISIONED" }
            for (kid in issuers.keys()) {
                check(kid.isNotBlank())
                val key = KeyFactory.getInstance("EC").generatePublic(X509EncodedKeySpec(SigningPolicy.decode(issuers.getString(kid)))) as ECPublicKey
                check(key.params.curve.field.fieldSize == 256 && key.params.order.bitLength() == 256)
            }
            probeSettings = NodeProbeSettings.parse(
                assets.open("test-probe.json").bufferedReader().use { it.readText() },
                assets.open("test-public-trust.json").bufferedReader().use { it.readText() },
            )
            // Optional non-personal mobile-v1 seed. Absent/invalid asset keeps the old flow.
            val mobileSeed = runCatching {
                assets.open("test-mobile.json").bufferedReader().use { it.readText() }
            }.getOrNull()
            val saved = storage.read()
            activeLink = link.ifEmpty { saved.optString("link") }
            val mobileBootstrap = MobileBootstrapGate.forLink(activeLink, MobileBootstrapSeed.parse(mobileSeed))
            val mobileBaseUrl = mobileBootstrap?.baseUrl
            // A linkless attempt is admitted only by the trusted packaged mobile seed: it is
            // the configured bootstrap input, not proof that the live transport works. An
            // arbitrary URL never qualifies, no pseudo-link is synthesized, and a saved
            // legacy link is never substituted for it. Legacy/import still needs a real link.
            check(MobileBootstrapGate.admits(activeLink, mobileBootstrap)) { "IMPORT_REQUIRED" }
            val network = if (requiredNetwork != null) {
                check(networkCandidate(requiredNetwork).usable) { "PHYSICAL_NETWORK_UNAVAILABLE" }
                requiredNetwork
            } else connectivity.allNetworks.filter { networkCandidate(it).usable }
                .maxByOrNull { networkCandidate(it).let { candidate ->
                    (if (candidate.isDefault) 2 else 0) + (if (candidate.validated) 1 else 0)
                } }
                ?: error("PHYSICAL_NETWORK_UNAVAILABLE")
            if (recoveryGeneration != null) {
                val recovery = networkRecovery ?: error("NETWORK_RECOVERY_STALE")
                check(recovery.generation == recoveryGeneration && recovery.nodeId == view.selectedNodeId) {
                    "NETWORK_RECOVERY_STALE"
                }
            }
            physical = network
            // Observation only: never change network selection or expose addresses/handles.
            val capabilities = runCatching { connectivity.getNetworkCapabilities(network) }.getOrNull()
            lastPhysicalNetwork = mapOf(
                "network_present" to (capabilities != null),
                "network_internet" to (capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) == true),
                "network_not_vpn" to (capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN) == true),
                "network_validated" to (capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED) == true),
                "network_default_match" to runCatching { connectivity.activeNetwork == network }.getOrDefault(false))
            val attempt = UUID.randomUUID().toString()
            if (catalogGate.beginCycle(attempt, catalogRequestId) == null) {
                completeCatalogRefresh(listOf(CatalogRefreshCoordinator.Completion(
                    catalogRequestId!!, false, "rejected")))
                return
            }
            startedAttempt = attempt
            lifecycleRevision.set(0)
            activeRuntimeEpoch = 0
            rollbackRuntimeEpoch = 0
            gate.start(attempt)
            publish(view.copy(attempt = attempt))
            // S3-B: a tap without a live attempt starts this bounded service-only attempt and
            // arms a finite window for the fresh confirmed /me before activation is sent.
            if (trialGate.onAttemptStarted(attempt)) {
                main.postDelayed({
                    if (gate.active == attempt && trialGate.onTimeout(attempt)) {
                        publish(view.copy(trial = (view.trial ?: TrialState()).copy(error = "TRIAL_CONTROL_TIMEOUT")))
                        stopAttempt("TRIAL_CONTROL_TIMEOUT")
                    }
                }, 15_000)
            }
            // S5: a payment tap without a live attempt starts this bounded service-only
            // attempt; the requested operation is sent exactly once after the first accepted
            // /me. A cold attempt that never delivers that first /me is released.
            if (purchaseGate.onAttemptStarted(attempt)) {
                main.postDelayed({
                    if (gate.active == attempt && purchaseGate.onTimeout(attempt)) {
                        publish(view.copy(purchase = PurchaseFlow.failure(view.purchase, "MOBILE_STATE_UNAVAILABLE")))
                        stopAttempt(null, "purchase_cold_timeout")
                    }
                }, PurchaseGate.COLD_WINDOW_MILLIS)
            }
            // §§18–19: a devices refresh/delete without a live attempt starts the same bounded
            // cold attempt; if no confirmed /me arrives in the window it is released truthfully.
            if (devicesGate.onAttemptStarted(attempt)) {
                main.postDelayed({
                    if (gate.active == attempt && !stopping.get() &&
                        devicesGate.isCold(attempt) && devicesGate.onTimeout(attempt)) {
                        publish(view.copy(devices = (view.devices ?: DevicesUi())
                            .copy(error = "SERVICE_UNAVAILABLE")))
                        stopAttempt(null, "devices_cold_timeout")
                    }
                }, PurchaseGate.COLD_WINDOW_MILLIS)
            }
            if (loginGate.onAttemptStarted(attempt)) {
                main.postDelayed({
                    if (gate.active == attempt && !stopping.get() &&
                        loginGate.isCold(attempt) && loginGate.onTimeout(attempt)) {
                        publish(view.copy(registration = (view.registration ?: RegistrationState())
                            .copy(error = "REGISTRATION_DISABLED")))
                        stopAttempt(null, "login_cold_timeout")
                    }
                }, PurchaseGate.COLD_WINDOW_MILLIS)
            }
            if (mobileBaseUrl != null) {
                // Exactly one bounded line per mobile attempt: public hex64 + normalized endpoint.
                android.util.Log.i(MobileAttemptDiagnostics.TAG,
                    MobileAttemptDiagnostics.line(storage.installationId(), mobileBaseUrl))
            }
            if (stopping.get()) { gate.cancel(); return }
            main.post {
                if (gate.active != attempt) return@post
                if (networkCallback == null) {
                    val callback = object : ConnectivityManager.NetworkCallback() {
                        override fun onLost(lost: Network) { main.post { handlePhysicalLoss(lost) } }
                        override fun onAvailable(network: Network) { main.post {
                            physicalAvailability.available(network)
                            handlePhysicalCandidate(network)
                        } }
                        override fun onCapabilitiesChanged(network: Network, capabilities: NetworkCapabilities) {
                            main.post { handlePhysicalCandidate(network) }
                        }
                    }
                    networkCallback = callback
                    connectivity.registerNetworkCallback(NetworkRequest.Builder()
                        .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                        .addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN).build(), callback)
                }
            }
            publishActive(attempt, ViewState("BootstrapConnecting",
                summary = if (activeLink == saved.optString("link")) view.summary else null,
                selectedNodeId = view.selectedNodeId,
                // The display-only browse list and the host-local preference survive the
                // new attempt start (owner contract 3: remembered across UI updates). An
                // unlinked mobile attempt starts in the browse display mode: until the
                // server answers with either branch, the retained verified catalog must
                // not arm a connect. A linked attempt keeps the credential mode so the
                // existing paid/retained connect path is not gated during the refresh;
                // a browse answer still switches the mode when rights are gone.
                browseNodes = view.browseNodes, browseLoaded = view.browseLoaded,
                browseSelectedId = view.browseSelectedId,
                displayMode = if (mobileBaseUrl != null && activeLink.isEmpty()) CatalogDisplayMode.BROWSE
                else CatalogDisplayMode.CREDENTIAL))
            val child = NativeProcess(
                java.io.File(applicationInfo.nativeLibraryDir, "libterlimo.so"), attempt, { event ->
                    receiveBridge(event, attempt)
                }, { code -> if (gate.active == attempt) handleAttemptTerminal(attempt, code) },
                { completion -> android.util.Log.w(ChildCompletionDiagnostics.TAG, ChildCompletionDiagnostics.line(completion)) },
                { code -> mirrorNativeStderr(attempt, code) })
            native = child
            val start = JSONObject().put("type", "start").put("link", activeLink)
                .put("public_key_spki", SigningPolicy.encode(storage.publicSpki()))
                .put("installation_id", storage.installationId())
                .put("physical_network_handle", network.networkHandle.toString())
                .put("issuers", issuers)
                .put("probe_by_node", probeSettings.toNativeJson())
            if (mobileBootstrap != null) {
                start.put("mobile_base_url", mobileBootstrap.baseUrl)
                start.put("mobile_environment", mobileBootstrap.environment)
                mobileBootstrap.serviceSeed?.let { start.put("service_seed", it) }
                storage.readAccountAccessState("accountaccess_receipt_v1")?.let {
                    start.put("accountaccess_state_b64", it)
                }
                storage.readAccountAccessState("service_seed_v1")?.let {
                    start.put("service_seed_state_b64", it)
                }
                storage.readAccountAccessState("onboarding_flow_v1")?.let {
                    start.put("onboarding_flow_state_b64", it)
                }
            }
            // Native validates subscription binding, preserving registration on same-subscription reimport.
            start.put("state_b64", saved.optString("state_b64"))
            if (gate.active != attempt || stopping.get()) return
            // Arm before any child start: native can accept a catalogue immediately, and a later
            // arm would resurrect a deadline that the accepted catalogue already disarmed.
            catalogTimer.apply(attempt, mobileCatalog.onAttemptStart(attempt, mobileBaseUrl != null))
            if (catalogRequestId == null) {
                child.start(start)
            } else if (!catalogGate.dispatch(attempt, catalogRequestId) { child.start(start); true }) {
                stopAttempt(null, "schedule_cancelled")
                return
            }
            child.send(JSONObject().put("type", if (getSystemService(PowerManager::class.java).isInteractive) "device_wake" else "device_sleep")
                .put("lifecycle_revision", lifecycleRevision.incrementAndGet()))
            // The explicit pre-admission first connect is consumed by the child that was
            // started for it; an attempt without it never carries the command.
            if (preAdmissionConnect) {
                preAdmissionConnect = false
                val gatewayKey = preAdmissionGatewayKey
                preAdmissionGatewayKey = ""
                if (gate.active == attempt && explicitConnect.arm(ExplicitConnectGate.Entry.PRE_ADMISSION, attempt)) {
                    child.send(explicitConnectCommand(gatewayKey))
                    android.util.Log.i("WDTT/Explicit", "stage=sent")
                }
            }
        } catch (e: Exception) {
            val code = hostFailureCode(e, "begin")
            if (trialGate.onControlFailed()) {
                publish(view.copy(trial = (view.trial ?: TrialState()).copy(error = "TRIAL_CONTROL_UNAVAILABLE")))
            }
            if (purchaseGate.onControlFailed()) {
                publish(view.copy(purchase = PurchaseFlow.failure(view.purchase, "MOBILE_STATE_UNAVAILABLE")))
            }
            if (startedAttempt != null) {
                completeCatalogRefresh(catalogGate.planTerminal(startedAttempt, code))
            } else {
                completeCatalogRefresh(listOfNotNull(catalogGate.failPendingCold("BEGIN_FAILED")))
            }
            if (recoveryGeneration != null) handleRecoveryTerminal(startedAttempt, recoveryGeneration, code)
            else if (activeVpnConfig != null) holdKillSwitch(code)
            else stopAttempt(code)
        }
    }
    private fun handle(event: JSONObject, attempt: String) {
        try {
            when (event.getString("type")) {
                "wake_status" -> {
                    val incoming = WakeRecoveryProjection.parse(event) ?: return
                    val current = view
                    val status = WakeRecoveryProjection.accept(current.wakeRecovery, incoming,
                        gate.active, attempt, activeRuntimeEpoch, lifecycleRevision.get(),
                        getSystemService(PowerManager::class.java).isInteractive, stopping.get())
                    if (status != current.wakeRecovery) publishActive(attempt, current.copy(wakeRecovery = status))
                }
                "channels_status" -> {
                    val incoming = ChannelsStatusProjection.parse(event) ?: return
                    val current = view
                    val status = ChannelsStatusProjection.accept(current.channels, incoming,
                        gate.active, attempt, activeRuntimeEpoch, lifecycleRevision.get(),
                        getSystemService(PowerManager::class.java).isInteractive, stopping.get())
                    if (status != current.channels) publishActive(attempt, current.copy(channels = status))
                }
                "diagnostic" -> {
                    retainManagedDiagnostic(event, false)
                }
                "account_access" -> {
                    // Read-only projection: malformed events are rejected without
                    // tearing down the attempt; the last good snapshot is kept.
                    val incoming = runCatching { AccountAccessParser.parse(event) }.getOrNull()
                    if (incoming == null) {
                        android.util.Log.w("WDTT/AccountAccess", "rejected")
                    } else {
                        val updated = AccountAccessPolicy.apply(view.accountAccess, incoming, SystemClock.elapsedRealtime())
                        if (updated != null) {
                            val registration = updated.projection.registration
                            val trial = updated.projection.trial
                            // S5 grant discipline: a paid purchase is confirmed only through
                            // this accepted fresh /me projection with an active entitlement.
                            // No payment status, redirect or checkout return writes access.
                            val previousPurchase = view.purchase
                            val nextPurchase = PurchaseFlow.onFreshMe(previousPurchase, updated.projection)
                            // A different account identity invalidates any device list and any
                            // outstanding delete correlation: a late reply must not apply to a
                            // different account. sessionGeneration is a reused counter and is
                            // deliberately NOT used as the account fence.
                            val accountChanged = view.accountAccess?.projection?.account?.accountRef !=
                                updated.projection.account.accountRef
                            publishActive(attempt, view.copy(
                                devices = if (accountChanged) DevicesPolicy.reset() else view.devices,
                                accountAccess = updated,
                                registration = RegistrationState(
                                    state = registration.state,
                                    withinHour = registration.withinHour,
                                    trialAvailable = registration.trialAvailable,
                                    trialReason = registration.trialReason,
                                    purchaseAvailable = registration.purchaseAvailable,
                                    error = null,
                                ),
                                trial = TrialState(
                                    state = trial.state,
                                    canActivate = trial.canActivate,
                                    reason = trial.reason,
                                    startsAt = trial.startsAt,
                                    endsAt = trial.endsAt,
                                    error = null,
                                ),
                                purchase = nextPurchase))
                            if (nextPurchase.phase == PurchaseFlow.CONFIRMED &&
                                previousPurchase?.phase != PurchaseFlow.CONFIRMED) {
                                // The purchase is confirmed by the server projection; the
                                // attempt identity is done, so the next purchase starts with
                                // a fresh key pair. The cold purchase attempt is released.
                                releaseConfirmedColdPurchase(attempt)
                                return
                            }
                            val pendingPurchase = purchaseGate.onVerifiedRights(attempt)
                            if (pendingPurchase != null) sendPurchaseOperation(attempt, pendingPurchase)
                            if (loginGate.onVerifiedRights(attempt)) {
                                native?.send(JSONObject().put("type", "request_telegram_registration"))
                            }
                            when (val devices = devicesGate.onVerifiedRights(
                                attempt, DevicesPolicy.canManage(registration))) {
                                is DevicesVerified.Send -> when (val request = devices.request) {
                                    is DevicesRequest.List -> requestDevicesList(attempt, request.requestId)
                                    is DevicesRequest.Delete -> performDeviceDelete(attempt, request)
                                }
                                DevicesVerified.Refused -> {
                                    publish(view.copy(devices = (view.devices ?: DevicesUi())
                                        .copy(error = "DEVICE_MANAGEMENT_FORBIDDEN")))
                                    if (devicesGate.onRefused(attempt)) stopAttempt(null, "devices_refused")
                                }
                                DevicesVerified.NotCold -> Unit
                            }
                            if (trialGate.onVerifiedRights(attempt, registration.state == "registered", trial.canActivate)) {
                                native?.send(JSONObject().put("type", "activate_trial"))
                            }
                            // Accepted CURRENT mobile projection drives the explicit per-attempt
                            // state machine: none/restricted_checkout -> WAITING_RIGHT (attempt
                            // stays alive, no catalogue timer, no VPN); a data right arms exactly
                            // one fresh finite window only on the transition into WAITING_CATALOG.
                            // Rejected/stale/foreign projections and legacy attempts change nothing.
                            val catalogAction =
                                mobileCatalog.onAccountAccess(updated.projection.grant.dataAccess, attempt)
                            catalogTimer.apply(attempt, catalogAction,
                                marker = if (catalogAction == MobileCatalogAction.DISARM) CatalogTimerMarker.DISARM else null,
                                detail = "rights_${updated.projection.grant.dataAccess}",
                            )
                            refreshAccessDisplay(attempt)
                            // §11: piggyback the accepted refresh slot; no new timer or poll.
                            requestAnnouncements()
                        }
                    }
                }
                "usage_result" -> {
                    // Account credited traffic (server truth), strictly distinct from the
                    // local live tunnel counters. A malformed/stale event keeps the last
                    // good projection; it never tears down the attempt or fabricates totals.
                    val parsed = runCatching {
                        UsageContract.parse(event, SystemClock.elapsedRealtime())
                    }.getOrNull()
                    when (parsed) {
                        is UsageEvent.Snapshot -> publishActive(attempt, view.copy(
                            serverUsage = parsed.usage, serverUsageUnavailable = false))
                        is UsageEvent.Failure -> {
                            android.util.Log.w("WDTT/Usage", "code=${parsed.code}")
                            publishActive(attempt, view.copy(
                                serverUsage = view.serverUsage?.copy(stale = true),
                                serverUsageUnavailable = view.serverUsage == null))
                        }
                        null -> android.util.Log.w("WDTT/Usage", "rejected")
                    }
                }
                "announcements_list" -> {
                    if (event.optString("state") == "ok") {
                        val incoming = runCatching { AnnouncementsCodec.parseList(event) }.getOrNull()
                        if (incoming == null) {
                            android.util.Log.w("WDTT/Announcements", "rejected")
                        } else {
                            val next = AnnouncementsPolicy.applyList(view.announcements, incoming)
                            if (next != view.announcements) publishActive(attempt, view.copy(announcements = next))
                        }
                    } else {
                        // A failed list keeps the last good snapshot; the bounded code is
                        // diagnostic only (never an invented status or count).
                        android.util.Log.w("WDTT/Announcements",
                            "state=" + event.optString("state") + " code=" + event.optString("code"))
                    }
                }
                "announcement_read" -> {
                    val token = view.announcements.pending?.token
                    if (event.optString("state") == "ok") {
                        val ack = runCatching { AnnouncementsCodec.parseRead(event) }.getOrNull()
                        if (ack != null && token != null) {
                            val next = AnnouncementsPolicy.ack(view.announcements, ack, token)
                            if (next != view.announcements) publishActive(attempt, view.copy(announcements = next))
                        }
                    } else if (token != null) {
                        val next = AnnouncementsPolicy.fail(view.announcements, token)
                        if (next != view.announcements) publishActive(attempt, view.copy(announcements = next))
                    }
                }
                "devices_list_result", "device_delete_result" -> {
                    val current = view.devices ?: DevicesUi()
                    val accountRef = view.accountAccess?.projection?.account?.accountRef
                    val rawRequestId = event.optString("client_request_id")
                    val parsedEvent = runCatching { DevicesContract.parse(event) }.getOrNull()
                    when (parsedEvent) {
                        is DevicesEvent.List -> {
                            // One correlated decision drives BOTH the publish and the stop: a
                            // foreign/late list can never stop the attempt.
                            val effect = DevicesPolicy.listEffect(
                                current, parsedEvent.clientRequestId, attempt, accountRef, parsedEvent.list)
                            if (effect != DevicesListEffect.IGNORED) {
                                val next = DevicesPolicy.applyList(
                                    current, parsedEvent.clientRequestId, attempt, accountRef, parsedEvent.list)
                                publishActive(attempt, view.copy(devices = next))
                                if (effect == DevicesListEffect.ACCEPTED_STOP && !stopping.get()) stopAttempt("DEVICE_REMOVED")
                                else if (!stopping.get() && devicesGate.onResult(attempt)) stopAttempt(null, "devices_result")
                            }
                        }
                        is DevicesEvent.Delete -> {
                            if (DevicesPolicy.deleteCorrelated(
                                    current, parsedEvent.result.clientRequestId, attempt, accountRef)) {
                                publishActive(attempt, view.copy(devices =
                                    DevicesPolicy.applyDelete(current, parsedEvent.result)))
                                // Reconcile slots/rows from the server; never decrement twice.
                                requestDevicesList(attempt)
                            }
                        }
                        is DevicesEvent.Failure -> {
                            if (parsedEvent.event == DevicesContract.EVENT_DELETE) {
                                val effect = DevicesPolicy.deleteEffect(
                                    current, parsedEvent.clientRequestId, attempt, accountRef, parsedEvent.code)
                                if (effect != DevicesListEffect.IGNORED) {
                                    publishActive(attempt, view.copy(devices = DevicesPolicy.releaseDelete(
                                        current, parsedEvent.clientRequestId, parsedEvent.code)))
                                    if (effect == DevicesListEffect.ACCEPTED_STOP && !stopping.get()) stopAttempt("DEVICE_REMOVED")
                                    else if (!stopping.get() && devicesGate.onResult(attempt)) stopAttempt(null, "devices_result")
                                }
                            } else {
                                // A list error clears only its own read token; any unrelated
                                // delete in flight is preserved. The stop is part of the same
                                // correlated decision (a foreign DEVICE_REMOVED never stops).
                                val effect = DevicesPolicy.listFailureEffect(
                                    current, parsedEvent.clientRequestId, attempt, accountRef, parsedEvent.code)
                                if (effect != DevicesListEffect.IGNORED) {
                                    val next = DevicesPolicy.applyListFailure(
                                        current, parsedEvent.clientRequestId, attempt, accountRef, parsedEvent.code)
                                    publishActive(attempt, view.copy(devices = next))
                                    if (effect == DevicesListEffect.ACCEPTED_STOP && !stopping.get()) stopAttempt("DEVICE_REMOVED")
                                    else if (!stopping.get() && devicesGate.onResult(attempt)) stopAttempt(null, "devices_result")
                                }
                            }
                        }
                        null -> {
                            // Malformed reply: correlate on the raw token before parse. Only the
                            // matching delete is released; a malformed list never clears it.
                            if (event.optString("type") == DevicesContract.EVENT_DELETE) {
                                if (DevicesPolicy.deleteCorrelated(current, rawRequestId, attempt, accountRef)) {
                                    publishActive(attempt, view.copy(devices =
                                        DevicesPolicy.releaseDelete(current, rawRequestId, "DEVICES_INVALID")))
                                    if (!stopping.get() && devicesGate.onResult(attempt)) stopAttempt(null, "devices_malformed")
                                }
                            } else {
                                val next = DevicesPolicy.applyListFailure(
                                    current, rawRequestId, attempt, accountRef, "DEVICES_INVALID")
                                if (next != current) {
                                    publishActive(attempt, view.copy(devices = next))
                                    if (!stopping.get() && devicesGate.onResult(attempt)) stopAttempt(null, "devices_malformed")
                                }
                            }
                            android.util.Log.w("WDTT/Devices", "rejected")
                        }
                    }
                }
                "telegram_registration" -> {
                    // One-time deep link from the native link request. The link is opened
                    // externally and never persisted; a repeated tap is idempotent.
                    val link = event.optString("deep_link")
                    if (event.optString("state") == "pending" && link.startsWith("https://t.me/")) {
                        openExternalLink(link)
                        // Published before a possible cold release: the final state must keep the
                        // pending receipt even when the service-only attempt is stopped here.
                        publish(view.copy(registration =
                            (view.registration ?: RegistrationState()).copy(state = "pending", error = null)))
                        if (loginGate.onResolved(attempt)) stopAttempt(null, "registration_link_result")
                    } else if (event.optString("state") == "error") {
                        publish(view.copy(registration =
                            (view.registration ?: RegistrationState()).copy(error = event.optString("code"))))
                        if (loginGate.onResolved(attempt)) stopAttempt(null, "registration_link_result")
                    }
                }
                "telegram_registration_status" -> {
                    publishActive(attempt, view.copy(registration = RegistrationState(
                        state = event.optString("state").ifEmpty { "none" },
                        withinHour = event.optBoolean("within_hour"),
                        trialAvailable = event.optBoolean("trial_available"),
                        trialReason = event.optString("trial_reason").ifEmpty { null },
                        purchaseAvailable = event.optBoolean("purchase_available"),
                        error = null,
                    )))
                }
                "trial_status" -> {
                    // Server-owned trial state or a fixed activation error. A replay/expired
                    // result is never shown as active; the authoritative /me refresh follows.
                    val stateValue = event.optString("state").ifEmpty { "none" }
                    val error = if (stateValue == "error") event.optString("code").ifEmpty { "TRIAL_FAILED" } else null
                    // The cold service-only attempt is released once its outcome is known; a
                    // pre-existing user attempt is never stopped here.
                    val stopCold = trialGate.onTerminal(attempt)
                    publishActive(attempt, view.copy(trial = TrialState(
                        state = if (stateValue == "error") (view.trial?.state ?: "none") else stateValue,
                        canActivate = if (stateValue == "error") (view.trial?.canActivate ?: false) else false,
                        reason = view.trial?.reason,
                        startsAt = event.optString("starts_at").ifEmpty { view.trial?.startsAt },
                        endsAt = event.optString("ends_at").ifEmpty { view.trial?.endsAt },
                        error = error,
                    )))
                    if (stopCold && error != null) stopAttempt(null, "trial_terminal")
                }
                "plans_list_result", "quote_create_result", "payment_create_result", "payment_get_result" ->
                    handlePurchaseEvent(event, attempt)
                "sign" -> sign(event, attempt)
                "persist" -> {
                    val bytes = SigningPolicy.decode(event.getString("state_b64"))
                    check(bytes.size <= 180_000)
                    bytes.fill(0)
                    val namespace = event.optString("namespace")
                    if (namespace.isEmpty()) {
                        storage.writeSubscriptionState(activeLink, event.getString("state_b64"))
                    } else {
                        storage.writeAccountAccessState(namespace, event.getString("state_b64"))
                    }
                    send(JSONObject().put("type", "persist_result").put("request_id", event.getString("request_id")))
                }
                "imported" -> {
                    storage.writeActiveLink(activeLink)
                    publishActive(attempt, view.copy(phase = "ImportVerified", error = null))
                }
                "state" -> {
                    val state = event.getString("state")
                    check(state in NATIVE_PHASES) { "BRIDGE_STATE_INVALID" }
                    val current = view
                    val updated = NodeSelection.applyNativePhase(current, state)
                    if (updated !== current) publishActive(attempt, updated)
                    if (updated.phase == "Connected") {
                        armTrafficTick()
                        requestUsageRead(attempt)
                        armUsageTick()
                    } else {
                        stopTrafficTick()
                        stopUsageTick()
                        trafficReadGate.bump()
                        val lastUsage = view.serverUsage
                        if (lastUsage != null && !lastUsage.stale) {
                            publishActive(attempt, view.copy(serverUsage = lastUsage.copy(stale = true)))
                        }
                    }
                }
                "catalog" -> {
                    if (BrowseCatalogCodec.isBrowse(event)) {
                        // Owner contract 2/3: the display branch is decided before the strict
                        // credential decoder. It never writes the verified cache/last-good/
                        // selection, never sets CatalogReady, never arms the catalogue deadline,
                        // probe/admission/sync and never starts intent/hour/VPN. A malformed
                        // browse answer fails the attempt through the existing host-failure path.
                        val updatedBrowse = BrowseCatalogCodec.apply(view, BrowseCatalogCodec.parse(event))
                        val refreshPlan = catalogGate.commit(attempt) { publishActive(attempt, updatedBrowse) }
                        if (refreshPlan.stopCycle) stopAttempt(null, "schedule_cancelled")
                        completeCatalogRefresh(refreshPlan.completions)
                        return
                    }
                    val catalog = NodeSelection.parseCatalog(event)
                    // Optional display data cannot change transport admission/outcome.
                    val summary = runCatching { CatalogSummary.parse(event) }.getOrNull()
                    val updated = NodeSelection.applyCatalog(view, catalog, summary)
                    // §26.5 commit-time ownership fence: the publish/persist block runs inside
                    // the ownership lock, so an Off/mode change either invalidates the claim
                    // before the decision or happens only after the write completed. A cycle
                    // that only served a canceled periodic request publishes nothing.
                    val refreshPlan = catalogGate.commit(attempt) {
                        // A fresh verified catalogue invalidates any in-flight "Пинг всех" run.
                        publishActive(attempt, updated.copy(pingAll = PingAllGate.reset()))
                        runCatching { persistCatalogCache(updated) }
                    }
                    if (!refreshPlan.publish) {
                        if (refreshPlan.stopCycle) stopAttempt(null, "schedule_cancelled")
                        return
                    }
                    completeCatalogRefresh(refreshPlan.completions)
                    // Fresh catalogue accepted for this attempt: terminal CATALOG_ACCEPTED, the
                    // acquisition deadline is done and later rights never re-arm it.
                    catalogTimer.apply(
                        attempt,
                        mobileCatalog.onCatalogAccepted(attempt),
                        marker = CatalogTimerMarker.ACCEPT,
                        detail = catalog.revision,
                    )
                    val recovery = networkRecovery
                    val armedTarget = holdFailover.armed
                    if (armedTarget != null) {
                        // Held-protection failover: select only the explicitly chosen node from
                        // the fresh verified catalog; never fall back to another node.
                        val target = HoldFailoverPolicy.catalogTarget(armedTarget, view.nodes.map { it.id }.toSet())
                        if (target != null) {
                            synchronized(holdLock) {
                                holdFailover = HoldFailoverPolicy.catalogAccepted(holdFailover, attempt, target)
                            }
                            send(JSONObject().put("type", "select_node").put("node_id", target))
                        } else {
                            clearHoldFailover()
                            handleAttemptTerminal(attempt, "NODE_UNAVAILABLE")
                        }
                    } else if (recovery != null && PhysicalNetworkRecovery.shouldSelectNode(recovery, attempt,
                        recoveryConnectAttempt, view.phase, view.selectedNodeId, view.nodes.map { it.id }.toSet())) {
                        recoveryConnectAttempt = attempt
                        send(JSONObject().put("type", "select_node").put("node_id", recovery.nodeId))
                    } else {
                        autoConnectRetained(attempt, updated)
                    }
                }
                "switch_diag" -> {
                    // Fixed, secret-free native bridge/runner stage; logged only when a switch runs.
                    check(event.keys().asSequence().toSet() == setOf("v", "attempt_id", "type", "stage")) {
                        "BRIDGE_MESSAGE_INVALID"
                    }
                    SwitchDiagnostics.log("native", event.getString("stage"))
                }
                "node_probe_result" -> {
                    // Strict envelope stays, extended only by the optional attribution id and
                    // the two measurements. The fence decision is pure (ProbeFrameHandler):
                    // only the matching echoed probe_id from the same attempt/runtime may
                    // settle the request, exactly once. rtt_ms is the only measured RTT
                    // (transport_setup_ms is diagnostic); ok without a truthful rtt_ms is
                    // Failed; busy keeps the matching request Running; malformed/stale drops.
                    val keys = event.keys().asSequence().toSet()
                    val required = setOf("v", "attempt_id", "type", "node_id", "status")
                    val optional = setOf("probe_id", "rtt_ms", "transport_setup_ms")
                    check(required.all(keys::contains) && keys.all { it in required || it in optional }) {
                        "BRIDGE_MESSAGE_INVALID"
                    }
                    val status = event.getString("status")
                    check(status in setOf("ok", "failed", "timeout", "busy", "cancelled")) {
                        "BRIDGE_MESSAGE_INVALID"
                    }
                    val id = event.getString("node_id")
                    // Missing/malformed probe_id fails closed: drop, no state change.
                    val rawProbeId = event.opt("probe_id")
                    val probeId = when (rawProbeId) {
                        null, JSONObject.NULL -> null
                        is String -> rawProbeId
                        else -> return
                    }
                    if (probeId != null && !ProbeIds.isValid(probeId)) return
                    val decision = probeFrames.onFrame(
                        nodeId = id,
                        probeId = probeId,
                        status = status,
                        rttMs = (event.opt("rtt_ms") as? Number)?.toLong(),
                        setupMs = (event.opt("transport_setup_ms") as? Number)?.toLong(),
                        attempt = attempt,
                        runtime = activeRuntimeEpoch,
                    )
                    val all = view.pingAll
                    if (decision is ProbeFrameDecision.Settle) {
                        // KeepRunning (busy) or Drop (stale/mismatched/malformed) falls through
                        // without touching any ping state.
                        val ping = decision.state
                        if (!all.active) {
                            // Single explicit point probe. A result for a node cancelled by
                            // the all-ping run is late and must not be applied as fresh.
                            if (!PingAllGate.isLateAfterCancel(view.pings, id)) {
                                publishActive(attempt, view.copy(pings = view.pings + (id to ping)))
                            }
                        } else if (view.phase !in setOf("CatalogReady", "Connected")) {
                            // The queue must not survive a phase change: stop it.
                            publishActive(attempt, view.copy(pingAll = PingAllGate.cancel(all)))
                        } else if (id != all.expectedId) {
                            // Late/foreign result: never advance or corrupt the run.
                        } else {
                            val step = PingAllGate.onResult(all, id)
                            val withPing = view.pings + (id to ping)
                            val next = step.startId
                            if (next == null) {
                                publishActive(attempt, view.copy(pings = withPing, pingAll = step.state))
                            } else {
                                val nextAttempt = gate.active
                                if (nextAttempt != null) {
                                    val now = SystemClock.elapsedRealtime()
                                    val nextProbeId = probeIds.nextManual()
                                    probeFrames.begin(ProbeFence(next, nextProbeId, nextAttempt, activeRuntimeEpoch))
                                    publishActive(attempt, view.copy(
                                        pings = withPing + (next to NodePingState.Running(now, now, now + 12_000, nextProbeId)),
                                        pingAll = step.state))
                                    native?.send(JSONObject().put("type", "probe_node").put("node_id", next).put("probe_id", nextProbeId))
                                }
                            }
                        }
                    }
                }
                "switch_result" -> {
                    check(SwitchResultFrame.accepted(event)) { "BRIDGE_MESSAGE_INVALID" }
                    val id = event.getString("node_id")
                    val switchId = event.getString("switch_id")
                    val revision = event.getString("catalog_revision")
                    check(view.nodes.any { it.id == id }) { "BRIDGE_MESSAGE_INVALID" }
                    if (view.pendingNodeId != id || view.pendingSwitchId != switchId ||
                        view.pendingSwitchRevision != revision) {
                        SwitchDiagnostics.log("result_received", "stale")
                        return
                    }
                    SwitchDiagnostics.log("result_received", if (event.getString("status") == "ok") "ok" else "failed")
                    when (event.getString("status")) {
                        "ok" -> {
                            check(view.phase == "SwitchingServer" && view.pendingNodeId == id && activeVpnNodeId == id) {
                                "BRIDGE_STATE_INVALID"
                            }
                            completeSwitch(attempt, id, switchId, revision)
                        }
                        "failed" -> {
                            val code = safeCode(event.optString("code"))
                            val rollbackAllowed = event.opt("rollback_allowed") as? Boolean
                                ?: error("BRIDGE_MESSAGE_INVALID")
                            if (!rollbackAllowed) stopAttempt(code)
                            else if (view.phase == "SwitchingServer") rollbackSwitch(attempt, id, switchId, revision, code)
                            else {
                                check(view.phase == "Connected" && view.pendingNodeId == id && activeVpnNodeId == view.selectedNodeId) {
                                    "BRIDGE_STATE_INVALID"
                                }
                                activeRuntimeEpoch = rollbackRuntimeEpoch
                                rollbackRuntimeEpoch = 0
                                publishActive(attempt, view.copy(wakeRecovery = null, channels = null, pendingNodeId = null, pendingSwitchId = null,
                                    pendingSwitchRevision = null, error = code))
                            }
                        }
                        else -> error("BRIDGE_MESSAGE_INVALID")
                    }
                }
                "vpn_config" -> configureVpn(event, attempt)
                "lease" -> gate.ifActive(attempt) { if (!stopping.get()) armExpiry(event) }
                "captcha" -> {
                    val id = event.getString("request_id")
                    val remaining = event.getLong("deadline_unix_ms") - System.currentTimeMillis()
                    check(remaining in 1..180_000 && captcha == null) { "CAPTCHA_INVALID" }
                    val url = event.getString("redirect_uri")
                    check(CaptchaActivity.allowedUri(url)) { "CAPTCHA_ORIGIN_INVALID" }
                    val prompt = CaptchaPrompt(attempt, id, url, SystemClock.elapsedRealtime() + remaining) { value ->
                        enqueueActive(attempt) {
                            val current = captcha
                            if (current?.attempt == attempt && current.id == id && gate.active == attempt) {
                                captcha = null
                                send(JSONObject().put("type", "captcha_result").put("request_id", id)
                                    .put("value", if (SystemClock.elapsedRealtime() >= current.deadlineElapsed) "error:timeout" else value))
                                publishActive(attempt, view.copy(phase = if (view.phase == "WaitingUser") "BootstrapConnecting" else view.phase, error = null))
                            }
                        }
                    }
                    gate.ifActive(attempt) { if (!stopping.get()) captcha = prompt }
                    main.postDelayed({ if (captcha === prompt) prompt.complete("error:timeout") }, remaining)
                    publishActive(attempt, view.copy(phase = if (view.phase == "Connected") "Connected" else "WaitingUser", error = "VK_CAPTCHA_REQUIRED"))
                }
                "error" -> stopAttempt(safeCode(event.optString("code")))
                "stopped" -> stopAttempt(null, "native_stopped")
                else -> error("BRIDGE_MESSAGE_INVALID")
            }
        } catch (e: Exception) {
            if (event.optString("type") == "vpn_config" && view.phase == "Connected" &&
                view.pendingNodeId == event.optString("node_id") &&
                view.pendingSwitchId == event.optString("switch_id") &&
                view.pendingSwitchRevision == event.optString("catalog_revision") &&
                activeVpnNodeId == view.selectedNodeId) {
                vpnApplying.set(false)
                runCatching { send(vpnResult(event, false, "VPN_READINESS_FAILED")) }
            } else stopAttempt(hostFailureCode(e, "bridge"))
        }
    }
    private fun hostFailureCode(error: Exception, boundary: String): String {
        val code = safeCode(error.message)
        // Record boundary/stage/attempt/safe code/exception class plus bounded code locations
        // only: exception messages and bridge payloads may contain secrets.
        val locations = error.stackTrace.filter { it.className.startsWith("xyz.terlimo.test.") }
            .take(4).joinToString(",") { "${it.className}.${it.methodName}:${it.lineNumber}" }
        android.util.Log.w("WDTT/HostFailure",
            FailureDiagnostics.line(boundary, view.phase, gate.active, code, error.javaClass.simpleName) +
                if (locations.isEmpty()) "" else " at=$locations")
        return code
    }
    /**
     * Display-only mapping of the mirrored, already-allowlisted native stderr codes. The
     * onboarding conflict terminal becomes a readable error state for the current attempt,
     * and a browse transport failure becomes an explicit offline state with retry; the
     * attempt lifecycle, admission and the durable gateway pairing are never changed here.
     */
    private fun mirrorNativeStderr(attempt: String, code: String) {
        val conflict = code == "onboarding:ONBOARDING_INTENT_CONFLICT"
        if (!conflict && code !in BrowseErrorPolicy.OFFLINE_CODES) return
        main.post {
            if (stopping.get() || gate.active != attempt) return@post
            if (conflict) {
                if (view.nodes.isEmpty()) publish(view.copy(error = "ONBOARDING_INTENT_CONFLICT"))
            } else {
                BrowseErrorPolicy.fromStderr(code, view)?.let { publish(view.copy(browseError = it)) }
            }
        }
    }
    /**
     * S5 explicit purchase operation. With a live attempt the operation is sent on the
     * existing child; otherwise one bounded service-only attempt is started and the
     * operation is sent exactly once after the first accepted /me on it. No background
     * or resume path starts anything: only the explicit UI tap.
     */
    /** One correlated devices list read (host-owned token + attempt/account scope). */
    private fun requestDevicesList(attempt: String, requestId: String = "devlist-" + java.util.UUID.randomUUID()) {
        if (!DevicesPolicy.canSendList(view.devices, requestId)) return
        val accountRef = view.accountAccess?.projection?.account?.accountRef
        val pending = DevicesPolicy.beginList(view.devices, requestId, attempt, accountRef)
        publish(view.copy(devices = pending))
        val sent = native?.trySend(org.json.JSONObject()
            .put("type", "devices_list").put("client_request_id", requestId)) == true
        if (!sent) {
            publish(view.copy(devices = DevicesPolicy.applyListFailure(
                pending, requestId, attempt, accountRef, "TRANSPORT")))
            if (devicesGate.onResult(attempt)) stopAttempt(null, "devices_send_failed")
        }
    }

    /**
     * A devices tap that found no live bridge starts one cold service-only attempt once the
     * previous attempt is fully torn down (a quick tap during teardown must not become a
     * generic transport error). Bounded local scheduling only; no new product timeout.
     */
    private fun startDevicesColdAttempt(remaining: Int = 8) {
        main.post {
            if (stopping.get()) {
                devicesGate.onTimeout(null)
                return@post
            }
            if (gate.active == null) {
                submitControl { begin("") }
                return@post
            }
            if (native != null || remaining <= 0) {
                devicesGate.onTimeout(null)
                publish(view.copy(devices = (view.devices ?: DevicesUi()).copy(error = "TRANSPORT")))
                return@post
            }
            main.postDelayed({ startDevicesColdAttempt(remaining - 1) }, 250)
        }
    }

    /** The one explicitly chosen delete; the host-owned request/key travel unchanged. */
    private fun performDeviceDelete(attempt: String, request: DevicesRequest.Delete) {
        val devices = view.devices ?: DevicesUi()
        val canManage = DevicesPolicy.canManage(view.accountAccess?.projection?.registration)
        if (!canManage || !DevicesPolicy.canSendDelete(devices, request.requestId)) return
        val accountRef = view.accountAccess?.projection?.account?.accountRef
        val pending = DevicesPolicy.beginDelete(devices, request.requestId, request.deviceId, attempt, accountRef)
        publish(view.copy(devices = pending))
        val sent = native?.trySend(org.json.JSONObject().put("type", "device_delete")
            .put("device_id", request.deviceId)
            .put("idempotency_key", request.idempotencyKey)
            .put("client_request_id", request.requestId)) == true
        if (!sent) {
            publish(view.copy(devices = DevicesPolicy.releaseDelete(pending, request.requestId, "TRANSPORT")))
            if (devicesGate.onResult(attempt)) stopAttempt(null, "devices_delete_send_failed")
        }
    }

    private fun handlePurchaseOperation(operation: PurchaseOperation) {
        if (stopping.get()) return
        // Single-flight: while one purchase request is outstanding a queued duplicate is not
        // sent (and rotates no key); the send path re-checks under the same serial control.
        if (purchaseFlight.busy()) {
            publish(view.copy(purchase = PurchaseFlow.sending(view.purchase)))
            return
        }
        when (purchaseGate.onTap(gate.active, operation)) {
            PurchaseTapAction.SEND_NOW -> {
                val attempt = gate.active ?: return
                submitControl {
                    if (gate.active == attempt && !stopping.get()) sendPurchaseOperation(attempt, operation)
                }
            }
            PurchaseTapAction.START_SERVICE -> submitControl { begin("") }
            PurchaseTapAction.ERROR ->
                publish(view.copy(purchase = PurchaseFlow.failure(view.purchase, "INVALID_REQUEST")))
            PurchaseTapAction.IGNORE -> Unit
        }
    }

    /**
     * The single host->native purchase message builder. Every quote/payment key comes from
     * the durable [PurchaseAttempts] policy and is persisted before this send; this method
     * never generates a key itself and never sends an action outside the frozen set.
     */
    private fun sendPurchaseOperation(attempt: String, operation: PurchaseOperation) {
        if (stopping.get() || gate.active != attempt) return
        // No live native connection: a local pre-send refusal, nothing reaches the wire and no
        // durable key is rotated.
        val connection = native ?: return
        val kind = when (operation) {
            PurchaseOperation.Plans -> PurchaseFlightKind.PLANS
            is PurchaseOperation.Quote -> PurchaseFlightKind.QUOTE
            is PurchaseOperation.Payment -> PurchaseFlightKind.PAYMENT
            is PurchaseOperation.PaymentGet -> PurchaseFlightKind.PAYMENT_GET
        }
        val flight = PurchaseFlight(
            kind = kind, attempt = attempt,
            quoteId = (operation as? PurchaseOperation.Payment)?.quoteId,
        )
        // One explicit request under the single-flight capture: a queued duplicate is refused
        // before any durable key is touched, and the write result is never an escaped exception.
        val outcome = purchaseSender.send(
            request = flight,
            prepare = {
                when (operation) {
                    PurchaseOperation.Plans ->
                        JSONObject().put("type", PaymentsContract.ACTION_PLANS_LIST)
                    is PurchaseOperation.Quote -> {
                        val before = purchaseAttempts.current()
                        val record = purchaseAttempts.beginQuote(
                            operation.planId, operation.durationCode, operation.method)
                        // A new quote attempt identity replaces the previous one: the old create
                        // correlation must not survive the replacement.
                        if (before?.attemptId != record.attemptId) paymentCreates.clear()
                        purchaseFlight.attachKey(flight, record.quoteKey)
                        JSONObject().put("type", PaymentsContract.ACTION_QUOTE_CREATE)
                            .put("plan_id", operation.planId).put("duration_code", operation.durationCode)
                            .put("method", operation.method).put("idempotency_key", record.quoteKey)
                    }
                    is PurchaseOperation.Payment -> {
                        val record = purchaseAttempts.beginPayment(operation.quoteId)
                        // Only this explicit send of this exact quote may later build an ack; the
                        // single-flight gate guarantees it is the only create on the stream.
                        paymentCreates.onSent(operation.quoteId)
                        purchaseFlight.attachKey(flight, record.paymentKey)
                        JSONObject().put("type", PaymentsContract.ACTION_PAYMENT_CREATE)
                            .put("quote_id", operation.quoteId).put("idempotency_key", record.paymentKey)
                    }
                    is PurchaseOperation.PaymentGet -> JSONObject()
                        .put("type", PaymentsContract.ACTION_PAYMENT_GET).put("payment_id", operation.paymentId)
                }
            },
            write = { connection.trySend(it) },
        )
        when (outcome) {
            PurchaseSendOutcome.WAITING ->
                publishActive(attempt, view.copy(purchase = PurchaseFlow.sending(view.purchase)))
            PurchaseSendOutcome.LOCAL_FAILURE -> {
                // Preparation failed after capture and nothing was written: no create correlation
                // may survive, and the capture was already freed by the sender.
                paymentCreates.clear()
                publishActive(attempt, view.copy(purchase = PurchaseFlow.failure(view.purchase, "TRANSPORT")))
            }
            PurchaseSendOutcome.WRITTEN ->
                publishActive(attempt, view.copy(purchase = PurchaseFlow.sending(view.purchase)))
            PurchaseSendOutcome.UNKNOWN_WRITE ->
                // Closed/partial/exception write: the request outcome is unknown, so the stream
                // is fenced by the standard terminal teardown (stopAttempt resets the holder and
                // the correlation). The durable idempotency keys are preserved for an idempotent
                // retry; no new create is released over a possibly written one.
                terminalFailure(attempt, "BRIDGE_WRITE_UNKNOWN")
        }
    }

    /**
     * Strict, display-only handling of the frozen payment result events. Malformed events are
     * rejected and the last good purchase state is kept. A paid payment triggers the existing
     * fresh /me refresh path (no new endpoint) and stays pending until that projection
     * confirms; nothing here writes entitlement, admission or grant state.
     */
    private fun handlePurchaseEvent(event: JSONObject, attempt: String) {
        val parsed = runCatching { PaymentsContract.parse(event) }.getOrElse {
            android.util.Log.w("WDTT/Payments", "rejected")
            return
        }
        // Release the single-flight holder only through the matching result/failure type of the
        // outstanding operation; a GET result can never release a create holder.
        when (parsed) {
            is PaymentsEvent.Plans -> purchaseFlight.releaseOn(PaymentsContract.TYPE_PLANS_LIST_RESULT)
            is PaymentsEvent.Quote -> purchaseFlight.releaseOn(PaymentsContract.TYPE_QUOTE_CREATE_RESULT)
            is PaymentsEvent.Payment -> purchaseFlight.releaseOn(parsed.type)
            is PaymentsEvent.Failure -> purchaseFlight.releaseOn(parsed.type)
        }
        when (parsed) {
            is PaymentsEvent.Failure -> {
                if (parsed.code == "IDEMPOTENCY_CONFLICT") {
                    purchaseAttempts.restart()
                    paymentCreates.clear()
                }
                // A failed create is an abandoned attempt: no later result may correlate.
                if (parsed.type == PaymentsContract.TYPE_PAYMENT_CREATE_RESULT) paymentCreates.clear()
                val failed = if (parsed.type == PaymentsContract.TYPE_PLANS_LIST_RESULT)
                    PurchaseFlow.plansFailure(view.purchase, parsed.code)
                else PurchaseFlow.failure(view.purchase, parsed.code)
                publishActive(attempt, view.copy(purchase = failed))
                if (purchaseGate.stopCold(attempt)) stopAttempt(null, "purchase_release")
            }
            is PaymentsEvent.Plans ->
                publishActive(attempt, view.copy(purchase =
                    PurchaseFlow.plansLoaded(view.purchase, parsed.plansRevision, parsed.plans)))
            is PaymentsEvent.Quote -> {
                purchaseAttempts.bindQuote(parsed.quote.quoteId)
                publishActive(attempt, view.copy(purchase = PurchaseFlow.quoteReady(view.purchase, parsed.quote)))
                if (purchaseGate.stopCold(attempt)) stopAttempt(null, "purchase_release")
            }
            is PaymentsEvent.Payment -> {
                // Only the payment_create_result of the explicitly sent operation may carry
                // the correlated acknowledgement; a get result never sets or refreshes it.
                val ack = paymentCreates.onCreateResult(parsed.type, parsed.payment)
                val terminal = parsed.payment.paymentStatus == "paid" ||
                    PurchaseFlow.terminalPayment(parsed.payment)
                // A terminal status tears the attempt down: no create correlation survives.
                if (terminal) paymentCreates.clear()
                val result = when {
                    terminal -> PurchaseFlow.paymentResult(view.purchase, parsed.payment)
                        .copy(createAck = null)
                    ack != null -> PurchaseFlow.paymentCreateResult(view.purchase, parsed.payment, ack)
                    else -> PurchaseFlow.paymentGetResult(view.purchase, parsed.payment)
                }
                publishActive(attempt, view.copy(purchase = result))
                when {
                    parsed.payment.paymentStatus == "paid" -> {
                        // Existing fresh-/me trigger (registration refresh command, no new
                        // endpoint); the entitlement is only updated by the resulting accepted
                        // account_access projection, never by this payment status.
                        native?.send(JSONObject().put("type", "refresh_telegram_registration"))
                        armPurchaseConfirmationWindow(attempt)
                    }
                    PurchaseFlow.terminalPayment(parsed.payment) -> {
                        purchaseAttempts.restart()
                        if (purchaseGate.stopCold(attempt)) stopAttempt(null, "purchase_release")
                    }
                    else -> if (purchaseGate.stopCold(attempt)) stopAttempt(null, "purchase_release")
                }
            }
        }
    }

    /** Releases a cold purchase attempt when the post-payment fresh /me did not arrive in time. */
    private fun armPurchaseConfirmationWindow(attempt: String) {
        if (!purchaseGate.isCold(attempt)) return
        main.postDelayed({
            if (gate.active == attempt && purchaseGate.stopCold(attempt)) stopAttempt(null, "purchase_confirmed")
        }, PurchaseGate.CONFIRMATION_WINDOW_MILLIS)
    }

    /**
     * A confirmed purchase ends its cold service-only attempt and its key identity. A
     * user-owned attempt (VPN/onboarding) is never stopped here: only the purchase attempt
     * released by [PurchaseGate.stopCold]. This is not the rights state machine.
     */
    private fun releaseConfirmedColdPurchase(attempt: String) {
        purchaseAttempts.restart()
        paymentCreates.clear()
        if (purchaseGate.stopCold(attempt)) stopAttempt(null, "purchase_release")
    }

    private fun sign(event: JSONObject, attempt: String) {
        val id = event.getString("signing_request_id")
        SigningPolicy.validateWorker(event.getString("kind"), event.getString("worker_id"))
        val deadline = event.getLong("deadline_unix_ms")
        if (!gate.admit(attempt, id, deadline, System.currentTimeMillis())) {
            send(JSONObject().put("type", "sign_result").put("signing_request_id", id).put("error", "SIGN_REJECTED"))
            return
        }
        try {
            signer.execute {
                val reply = JSONObject().put("type", "sign_result").put("signing_request_id", id)
                val transcript = runCatching { SigningPolicy.decode(event.getString("transcript_b64")) }.getOrNull()
                try {
                    check(gate.active == attempt && System.currentTimeMillis() < deadline) { "SIGN_CANCELLED" }
                    check(transcript != null) { "SIGN_TRANSCRIPT_INVALID" }
                    reply.put("signature_b64", SigningPolicy.encode(storage.sign(event.getString("kind"), transcript)))
                } catch (_: Exception) { reply.put("error", "KEY_OR_SIGN_UNAVAILABLE") }
                finally { transcript?.fill(0) }
                if (System.currentTimeMillis() >= deadline) {
                    reply.remove("signature_b64"); reply.put("error", "SIGN_EXPIRED")
                }
                if (gate.finish(attempt, id)) runCatching { send(reply) }
            }
        } catch (_: java.util.concurrent.RejectedExecutionException) {
            gate.finish(attempt, id)
            send(JSONObject().put("type", "sign_result").put("signing_request_id", id).put("error", "SIGN_BUSY"))
        }
    }
    private fun armExpiry(event: JSONObject) {
        val lifetime = AccessLifetimeParser.parse(event)
        if (lifetime.remainingMillis == null) {
            main.removeCallbacks(expiry)
            leaseAlarms.cancel(0)
            expiresElapsed = Long.MAX_VALUE
            leaseEnd = "unlimited"
            return
        }
        val remaining = lifetime.remainingMillis
        val end = requireNotNull(lifetime.expiresAt)
        val candidate = SystemClock.elapsedRealtime() + remaining
        expiresElapsed = LeaseExpiryPolicy.merge(expiresElapsed, leaseEnd == end.toString(), candidate)
        leaseEnd = end.toString()
        val delay = expiresElapsed - SystemClock.elapsedRealtime()
        check(delay > 0) { "LEASE_EXPIRED" }
        scheduleLeaseExpiry(delay)
    }
    private fun configureVpn(event: JSONObject, attempt: String) {
        val runtimeEpoch = wakeInteger(event.opt("runtime_epoch")) ?: error("BRIDGE_MESSAGE_INVALID")
        val nodeId = event.getString("node_id")
        val switchId = event.optString("switch_id")
        val switchRevision = event.optString("catalog_revision")
        val switching = activeVpnConfig != null && ActiveNodeSwitch.awaitingConfig(
            view, activeVpnNodeId, nodeId, switchId, switchRevision)
        // Held-protection failover: a fresh attempt for an explicitly chosen node. The retained
        // tunnel keeps protecting until this apply succeeds; an apply failure holds, never tears down.
        val holdSelect = holdFailover.attempt == attempt && holdFailover.nodeId == nodeId
        check((switching || holdSelect || view.phase != "Connected") && vpnApplying.compareAndSet(false, true)) { "VPN_ALREADY_ACTIVE" }
        if (!switching && (!gate.ifActive(attempt) { if (!stopping.get()) armExpiry(event) } || stopping.get())) {
            vpnApplying.set(false)
            return
        }
        check(nodeId.isNotEmpty() && view.nodes.any { it.id == nodeId } &&
            (switching || holdSelect || (view.pendingNodeId == null && nodeId == view.selectedNodeId))) { "BRIDGE_MESSAGE_INVALID" }
        if (switching) AccessLifetimeParser.parse(event)
        val selectedProbe = probeSettings[nodeId] ?: error("PROBE_NOT_PROVISIONED")
        val config = Config.parse(ByteArrayInputStream(event.getString("config").toByteArray()))
        check(config.peers.size == 1 && config.`interface`.dnsServers.isNotEmpty()) { "VPN_CONFIG_INVALID" }
        check(config.`interface`.excludedApplications.isEmpty() && config.`interface`.includedApplications.isEmpty()) { "VPN_CONFIG_INVALID" }
        TestVpnPolicy.requireIpv4(
            config.`interface`.addresses.map { it.address },
            config.`interface`.dnsServers,
            config.peers.single().allowedIps.map { it.address })
        val endpoint = config.peers.single().endpoint.orElseThrow()
        check(endpoint.host == "127.0.0.1" && endpoint.port in 1..65535) { "VPN_ENDPOINT_INVALID" }
        check(config.peers.single().allowedIps.any { it.mask == 0 && it.address is java.net.Inet4Address }) { "VPN_DEFAULT_ROUTE_REQUIRED" }
        check(selectedProbe.probeUrl == event.getString("probe_url") &&
            selectedProbe.expectedExitIp == event.getString("expected_exit_ip")) { "PROBE_NOT_PROVISIONED" }
        val probeUrl = URL(selectedProbe.probeUrl)
        publishActive(attempt, view.copy(phase = if (switching) "SwitchingServer" else "ConfiguringVPN", wakeRecovery = null, channels = null, error = null))
        vpnWorker.execute {
            var failedCode = "VPN_APPLY_FAILED"
            var evidence = ReadinessEvidence()
            val previousConfig = activeVpnConfig
            val previousPlan = activeRoutingPlan
            val previousNodeId = activeVpnNodeId
            val previousExpiresElapsed = expiresElapsed
            val previousLeaseEnd = leaseEnd
            val previousBackend = backend
            var candidateBackend: SafeGoBackend? = null
            try {
                check(gate.active == attempt)
                val wireguard = if (switching) SafeGoBackend(this).also { candidateBackend = it }
                    else backend ?: SafeGoBackend(this).also { backend = it }
                wireguard.setRevocationListener { stopAttempt("VPN_REVOKED") }
                wireguard.setPhysicalNetwork(physical)
                val protectedPackages = setOf(packageName)
                val presets = QuickExclusionCatalog(emptyList())
                val settings = storage.readRoutingSettings(protectedPackages, presets)
                val runtimePlan = settings?.let { document ->
                    RoutingRuntimePlanner.plan(document,
                        config.`interface`.dnsServers.map { requireNotNull(it.hostAddress) },
                        packageManager.getInstalledApplications(0).map { it.packageName }.toSet(),
                        protectedPackages)
                }
                val vpnPlan = runtimePlan?.let { plan ->
                    RoutingVpnPlan(
                        plan.includedApplications,
                        plan.excludedApplications,
                        plan.ipv4Routes.map(InetNetwork::parse),
                        plan.dnsServers.map(java.net.InetAddress::getByName),
                    )
                }
                wireguard.setRoutingPlan(vpnPlan)
                if (switching) {
                    rollbackVpnConfig = previousConfig
                    rollbackRoutingPlan = previousPlan
                    rollbackVpnNodeId = previousNodeId
                    rollbackBackend = previousBackend
                }
                val applyStarted = System.currentTimeMillis()
                wireguard.setState(tunnel, Tunnel.State.UP, config)
                check(gate.active == attempt)
                failedCode = "VPN_PROBE_FAILED"
                val probe = VpnReadinessProbe(connectivity,
                    config.`interface`.addresses.map { it.address }.toSet(),
                    (vpnPlan?.dnsServers ?: config.`interface`.dnsServers).toSet(),
                    probeUrl, selectedProbe.expectedExitIp, { gate.active == attempt && !stopping.get() }) { sample ->
                    val stats = wireguard.getStatistics(tunnel)
                    val peer = stats.peers().singleOrNull()?.let { stats.peer(it) }
                    sample.statsOk = peer != null
                    sample.rx = peer?.rxBytes ?: 0; sample.tx = peer?.txBytes ?: 0
                    val handshake = peer?.latestHandshakeEpochMillis ?: 0
                    sample.handshakePresent = handshake > 0
                    sample.handshakeFresh = handshake >= applyStarted && handshake <= System.currentTimeMillis()
                }
                readinessProbe = probe
                evidence = probe.run(minOf(SystemClock.elapsedRealtime() + 15_000, expiresElapsed,
                    if (switching) Long.MAX_VALUE else connectDeadline.deadline(attempt)))
                lastReadiness = evidence.snapshot(probe.expired) + mapOf("elapsed_ms" to probe.elapsedMs,
                    "owner_uid_check_available" to (Build.VERSION.SDK_INT >= 30))
                readinessProbe = null
                failedCode = if (probe.expired && evidence.ready) "VPN_READINESS_TIMEOUT" else evidence.failureCode()
                // Fixed, secret-free readiness sub-cause line (stage/exception/code + facts).
                // Diagnostic only; readiness/timeout/endpoint/admission are unchanged.
                if (!evidence.ready) {
                    android.util.Log.w("WDTT/Readiness", ReadinessDiagnostics.line(
                        evidence.stage.name, evidence.exceptionClass.name, failedCode,
                        evidence.dnsOk, evidence.httpsOk, evidence.httpStatusOk, evidence.expectedExitOk,
                        probe.elapsedMs, probe.expired))
                }
                check(evidence.ready && !probe.expired && gate.active == attempt && SystemClock.elapsedRealtime() < expiresElapsed)
                if (switching) {
                    rollbackExpiresElapsed = previousExpiresElapsed
                    rollbackLeaseEnd = previousLeaseEnd
                    armExpiry(event)
                }
                check(gate.ifActive(attempt) { check(!stopping.get()); activeRuntimeEpoch = runtimeEpoch })
                activeVpnConfig = config
                activeRoutingPlan = vpnPlan
                activeVpnNodeId = nodeId
                if (holdSelect) clearHoldFailover()
                if (switching) backend = wireguard
                val traffic = if (switching) {
                    // In-place switch keeps the accumulated totals and re-baselines only.
                    trafficSampler.onRecoveryOrSwitch()
                } else {
                    // Explicit user Connect after OFF starts a fresh session.
                    trafficSampler.onExplicitConnect()
                }
                trafficReadGate.bump()
                if (gate.active == attempt && !stopping.get()) {
                    publishActive(attempt, view.copy(traffic = traffic))
                }
                send(vpnResult(event, true, null))
                if (!switching) {
                    networkRecovery?.let { recovery ->
                        check(PhysicalNetworkRecovery.established(recovery, recovery.generation, nodeId)) {
                            "NETWORK_RECOVERY_STALE"
                        }
                        networkRecovery = null
                        recoveryNetwork = null
                        cancelScheduledRecovery()
                        main.removeCallbacks(recoveryExpiry)
                    }
                    gate.ifActive(attempt) {
                        if (!stopping.get()) {
                            if (connectDeadline.complete(attempt, SystemClock.elapsedRealtime()))
                                publish(view.copy(phase = "Connected", selectedNodeId = nodeId, pendingNodeId = null, error = null))
                            else handleAttemptTerminal(attempt, "VPN_SETUP_TIMEOUT")
                        }
                    }
                }
            } catch (error: Exception) {
                readinessProbe?.close(); readinessProbe = null
                if (lastReadiness.isEmpty()) { evidence.recordException(error); lastReadiness = evidence.snapshot() }
                if (gate.active == attempt) {
                    runCatching { candidateBackend?.retireState(tunnel) }
                    runCatching { send(vpnResult(event, false, "VPN_READINESS_FAILED")) }
                    val terminalCode = if (failedCode == "READY") "LEASE_EXPIRED" else failedCode
                    if (switching && previousConfig != null) {
                        backend = previousBackend
                        activeVpnConfig = previousConfig
                        activeRoutingPlan = previousPlan
                        activeVpnNodeId = previousNodeId
                        publishActive(attempt, view.copy(phase = "SwitchingServer", selectedNodeId = previousNodeId,
                            pendingNodeId = nodeId, error = null))
                    } else if (networkRecovery != null || activeVpnConfig != null) {
                        // A resumed recovery or held-protection attempt failed to apply: never
                        // tear down the retained protection on an apply failure. The terminal
                        // policy keeps the TUN (HOLD) or continues the bounded recovery.
                        handleAttemptTerminal(attempt, terminalCode)
                    } else stopAttempt(terminalCode)
                }
            } finally {
                vpnApplying.set(false)
            }
        }
    }
    private fun publishActive(attempt: String, state: ViewState) {
        gate.ifActive(attempt) { if (!stopping.get()) publish(state) }
    }
    /** Arms or cancels the display-only onboarding-hour countdown tick; never gates access. */
    private fun refreshAccessDisplay(attempt: String) {
        main.removeCallbacks(accessTick)
        if (AccountAccessDisplayRefresh.shouldPost(
                gate.active == attempt && !stopping.get(), view.accountAccess, SystemClock.elapsedRealtime()))
            main.postDelayed(accessTick, AccountAccessDisplayRefresh.TICK_MILLIS)
    }
    private fun vpnResult(event: JSONObject, ok: Boolean, error: String?): JSONObject =
        JSONObject().put("type", "vpn_result").put("request_id", event.getString("request_id"))
            .put("runtime_epoch", requireNotNull(wakeInteger(event.opt("runtime_epoch")))).put("ok", ok).also { result ->
            if (event.has("switch_id")) result.put("switch_id", event.getString("switch_id"))
                .put("catalog_revision", event.getString("catalog_revision"))
            if (error != null) result.put("error", error)
        }
    private fun completeSwitch(attempt: String, targetId: String, switchId: String, revision: String) {
        val oldBackend = rollbackBackend
        vpnWorker.execute {
            val clean = runCatching { oldBackend?.retireState(tunnel) }.isSuccess
            if (gate.active != attempt || stopping.get()) return@execute
            if (!clean) { stopAttempt("CLEANUP_FAILED"); return@execute }
            clearRollback()
            publishActive(attempt, ActiveNodeSwitch.success(view, activeVpnNodeId, targetId, switchId, revision))
        }
    }
    private fun rollbackSwitch(attempt: String, targetId: String, switchId: String, revision: String, code: String) {
        val previousConfig = rollbackVpnConfig
        val previousPlan = rollbackRoutingPlan
        val previousNode = rollbackVpnNodeId
        val previousBackend = rollbackBackend
        check(view.phase == "SwitchingServer" && view.pendingNodeId == targetId && previousConfig != null && previousNode.isNotEmpty()) {
            "BRIDGE_STATE_INVALID"
        }
        vpnWorker.execute {
            val restored = runCatching {
                val current = backend
                if (current !== previousBackend) current?.retireState(tunnel)
                val wireguard = checkNotNull(previousBackend)
                restoreVpn(wireguard, previousConfig, previousPlan)
                restoreLease(rollbackExpiresElapsed, rollbackLeaseEnd)
            }.isSuccess
            if (gate.active != attempt || stopping.get()) return@execute
            if (!restored) {
                stopAttempt("CLEANUP_FAILED")
                return@execute
            }
            activeVpnConfig = previousConfig
            activeRoutingPlan = previousPlan
            activeVpnNodeId = previousNode
            backend = previousBackend
            activeRuntimeEpoch = rollbackRuntimeEpoch
            clearRollback()
            publishActive(attempt, ActiveNodeSwitch.failure(view, previousNode, targetId, switchId, revision, code))
        }
    }
    private fun clearRollback() {
        rollbackRuntimeEpoch = 0
        rollbackVpnConfig = null
        rollbackRoutingPlan = null
        rollbackVpnNodeId = ""
        rollbackBackend = null
        rollbackExpiresElapsed = 0
        rollbackLeaseEnd = ""
    }
    private fun restoreVpn(wireguard: SafeGoBackend, config: Config, plan: RoutingVpnPlan?) {
        wireguard.retireState(tunnel)
        wireguard.setRoutingPlan(plan)
        wireguard.setState(tunnel, Tunnel.State.UP, config)
        check(wireguard.getState(tunnel) == Tunnel.State.UP)
    }
    private fun restoreLease(deadlineElapsed: Long, end: String) {
        expiresElapsed = deadlineElapsed
        leaseEnd = end
        main.removeCallbacks(expiry)
        if (end == "unlimited" && deadlineElapsed == Long.MAX_VALUE) {
            leaseAlarms.cancel(0)
            return
        }
        val remaining = deadlineElapsed - SystemClock.elapsedRealtime()
        check(remaining > 0) { "LEASE_EXPIRED" }
        scheduleLeaseExpiry(remaining)
    }
    private fun scheduleLeaseExpiry(delay: Long) {
        // Alarm delivery can be delayed by Android. Native/server expiry guards
        // remain authoritative for traffic; this timer updates/stops the host.
        leaseAlarms.schedule(0, expiresElapsed)
        main.removeCallbacks(expiry)
        main.postDelayed(expiry, delay)
    }
    private fun enqueueActive(attempt: String, action: () -> Unit) {
        if (gate.active != attempt || stopping.get()) return
        submitControl { if (gate.active == attempt && !stopping.get()) action() }
    }
    private fun submitControl(action: () -> Unit) {
        if (stopping.get()) return
        if (actor.submit { if (!stopping.get()) action() } == BridgeActor.Result.FULL) {
            // A wedged control pipe is an unexpected terminal failure: while the tunnel is
            // applied the protection is held instead of releasing traffic.
            terminalFailure(gate.active, "BRIDGE_CONTROL_OVERFLOW")
        }
    }
    /**
     * The only entry point that may start the held failover attempt. It runs on the actor and
     * re-checks lease/stopping/target/protection and the confirmed previous-child stop before
     * starting exactly one fresh native attempt; otherwise the pending choice is cancelled.
     */
    private fun startHeldFailover(target: String) {
        submitControl {
            val now = SystemClock.elapsedRealtime()
            val state = synchronized(holdLock) { holdFailover }
            if (state.armed == target && gate.active == null && !stopping.get() &&
                recoveryNativeStopped && activeVpnConfig != null && view.phase == "KillSwitch" &&
                view.nodes.any { it.id == target } && !LeaseExpiryPolicy.due(expiresElapsed, now)) {
                begin("")
            } else if (state.armed == target || state.wait?.target == target) {
                synchronized(holdLock) {
                    if (holdFailover.armed == target || holdFailover.wait?.target == target) {
                        holdFailover = HoldFailoverPolicy.cleared()
                    }
                }
            }
        }
    }
    /**
     * Completion of the previous native child stop. Runs on main; only the pending generation may
     * start, a failed/unconfirmed stop or a stale completion never starts a new child.
     */
    private fun completeHoldFailover(stopGeneration: Long, stopConfirmed: Boolean) {
        var startTarget: String? = null
        synchronized(holdLock) {
            val wait = holdFailover.wait ?: return
            val (next, decision) = HoldFailoverPolicy.childStopped(holdFailover, stopGeneration, stopConfirmed,
                stopping.get(), view.phase, activeVpnConfig != null, view.nodes.any { it.id == wait.target },
                !LeaseExpiryPolicy.due(expiresElapsed, SystemClock.elapsedRealtime()))
            holdFailover = next
            if (decision is HoldFailoverStart.Start) startTarget = decision.target
        }
        startTarget?.let(::startHeldFailover)
    }
    private fun receiveBridge(event: JSONObject, attempt: String) {
        if (gate.active != attempt || stopping.get()) return
        val type = event.optString("type")
        if (type == "bootstrap_diagnostic") {
            // A terminal event bypasses the actor; save its preceding tiny snapshot here.
            // Invalid telemetry is ignored, never turned into a functional bridge failure.
            val snapshot = BootstrapDiagnostics.parse(event, attempt)
            if (snapshot != null) gate.ifActive(attempt) {
                if (!stopping.get()) lastBootstrap = BootstrapDiagnostics.retainFirstFailure(lastBootstrap, snapshot)
            }
            return
        }
        if (type == "diagnostic" && event.opt("vpn_terminal") == true) {
            // Final closed evidence must be retained before the following
            // terminal frame bypasses and closes the actor queue.
            runCatching { retainManagedDiagnostic(event, true) }
            return
        }
        // Terminal notifications, like the owner's Cancel, never queue behind ingress.
        if (type == "error" || type == "stopped") {
            val code = if (type == "error") safeCode(event.optString("code")) else null
            if (code == null) stopAttempt(null, if (type == "error") "native_error" else "native_stopped")
            else handleAttemptTerminal(attempt, code)
            return
        }
        val result = actor.submit(BridgeActor.BACKPRESSURE_MS) {
            if (gate.active == attempt && !stopping.get()) handle(event, attempt)
        }
        if (result == BridgeActor.Result.FULL && gate.active == attempt && !stopping.get()) {
            // Never send a refusal on the reader: a full opposite pipe could
            // deadlock both directions. Typed terminal failure closes native waiters,
            // retains durable pending state and never manufactures a success ACK.
            terminalFailure(attempt, BridgeActor.overflowCode(type))
        }
    }
    /**
     * One entry point for unexpected terminal failures. An explicit Disconnect/"stopped" and an
     * OS revoke keep their own paths; everything else goes through [TerminalFailurePolicy], so an
     * applied tunnel is held (KillSwitch) rather than torn down into a direct exit. A failure
     * tagged with a different attempt is stale and never touches the current attempt; a failure
     * without a live attempt still cannot bypass the policy when protection is applied.
     */
    private fun terminalFailure(attempt: String?, code: String) {
        val active = gate.active
        when (TerminalFailurePolicy.decide(attempt, active, code, networkRecovery != null, activeVpnConfig != null)) {
            TerminalDecision.IGNORE_STALE -> return
            TerminalDecision.STOP -> stopAttempt(code)
            TerminalDecision.HOLD -> holdKillSwitch(code)
            TerminalDecision.RETRY_RECOVERY -> active?.let { handleAttemptTerminal(it, code) }
        }
    }
    private fun send(message: JSONObject) { if (!stopping.get() && gate.active != null) native?.send(message) }

    /**
     * Opens the one-time Telegram registration deep link externally. Only the fixed t.me
     * link is accepted; it is never persisted and no other URI scheme is started.
     */
    private fun openExternalLink(link: String) {
        if (!link.startsWith("https://t.me/")) return
        val intent = Intent(Intent.ACTION_VIEW, android.net.Uri.parse(link))
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        runCatching { startActivity(intent) }
    }

    /**
     * Truthful switch-only write result: `ok` only when the line was really written and flushed
     * to the child stdin. Failure reasons are fixed tokens; existing [send] is unchanged.
     */
    private fun sendSwitchNode(message: JSONObject): String {
        if (stopping.get()) return "closing"
        if (gate.active == null) return "gate"
        val child = native ?: return "no_native"
        return if (child.trySend(message)) "ok" else "write"
    }
    private fun retainManagedDiagnostic(event: JSONObject, terminal: Boolean) {
        val envelope = RELAY_FIELDS + VPN_DIAGNOSTIC_FIELDS + setOf("v", "attempt_id", "type") +
            if (terminal) setOf("vpn_terminal") else emptySet()
        check(event.keys().asSequence().toSet() == envelope && (!terminal || event.opt("vpn_terminal") == true)) {
            "BRIDGE_DIAGNOSTIC_INVALID"
        }
        lastRelay = RELAY_FIELDS.associateWith { key ->
            event.getLong(key).also { check(it >= 0) { "BRIDGE_DIAGNOSTIC_INVALID" } }
        }
        lastVpnDiagnostic = VpnDiagnostics.parse(event.opt("vpn_stage"), event.opt("vpn_error_class"), event.opt("vpn_auth_code"))
            ?: error("BRIDGE_DIAGNOSTIC_INVALID")
    }
    private fun networkCandidate(network: Network): NetworkCandidate {
        if (!physicalAvailability.accepts(network)) return NetworkCandidate(false, false, false, false)
        val capabilities = runCatching { connectivity.getNetworkCapabilities(network) }.getOrNull()
        return NetworkCandidate(
            capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) == true,
            capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN) == true,
            capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED) == true,
            runCatching { connectivity.activeNetwork == network }.getOrDefault(false),
        )
    }
    private fun pauseForSleep() {
        if (stopping.get() || sleepPaused || view.phase != "Connected" || native == null) return
        sleepPaused = true
        sleepResumeRequested = false
        recoveryNativeStopped = false
        val child = native
        android.util.Log.w("WDTT/Teardown", "stage=clear_native site=sleep child=" + (child != null))
        native = null
        activeRuntimeEpoch = 0
        gate.cancel()
        readinessProbe?.close(); readinessProbe = null
        publish(view.copy(phase = "SleepPaused", attempt = null, wakeRecovery = null, channels = null, error = null))
        val stopPhase = view.phase
        Thread({
            runCatching { child?.stop(ChildStopReason.SLEEP_PAUSE, stopPhase) }
            main.post {
                if (stopping.get()) return@post
                recoveryNativeStopped = true
                if (sleepResumeRequested) resumeFromSleep()
            }
        }, "terlimo-sleep-pause").start()
    }
    private fun resumeFromSleep() {
        if (stopping.get() || !sleepPaused) return
        sleepResumeRequested = true
        if (!recoveryNativeStopped) return
        sleepPaused = false
        sleepResumeRequested = false
        beginPhysicalRecovery()
    }
    private fun handlePhysicalLoss(lost: Network) {
        // onLost is authoritative even while capabilities/allNetworks are stale.
        // Only a later onAvailable may admit this handle again.
        physicalAvailability.lost(lost)
        if (recoveryNetwork == lost) {
            recoveryNetwork = null
            cancelScheduledRecovery()
        }
        if (lost == physical) {
            physical = null
            if (sleepPaused) retention.update(false, true, null)
            else beginPhysicalRecovery()
        }
        if (networkRecovery != null && recoveryNativeStopped && gate.active == null) {
            connectivity.allNetworks.firstOrNull { networkCandidate(it).usable }?.let(::handlePhysicalCandidate)
        }
    }
    private fun handlePhysicalCandidate(network: Network) {
        val candidate = networkCandidate(network)
        if (sleepPaused) {
            if (physical == null && candidate.usable) physical = network
            retention.update(false, true, physical)
            return
        }
        if (network == physical && PhysicalNetworkRecovery.boundPathUnavailable(candidate)) {
            beginPhysicalRecovery()
            return
        }
        if (!candidate.usable) return
        // Merely observing another candidate must not stop the healthy bound
        // transport. onLost/actual capability loss owns physical recovery.
        val recovery = networkRecovery ?: return
        val now = SystemClock.elapsedRealtime()
        if (recovery.awaitingNetwork || PhysicalNetworkRecovery.expired(recovery, now)) {
            // The bounded series is over: resume the SAME node with a fresh generation once a
            // usable physical network returns. A stopped owner, stale generation, unusable
            // network or expired/revoked right never restarts anything.
            if (!recoveryNativeStopped || recoveryScheduled || recovery.inFlight || gate.active != null) return
            val start = PhysicalNetworkRecovery.resume(recovery, recovery.generation, candidate, now,
                leaseValid = !LeaseExpiryPolicy.due(expiresElapsed, now), stopping = stopping.get()) ?: return
            recoveryNetwork = network
            networkRecovery = start.state
            networkGeneration = maxOf(networkGeneration, start.state.generation)
            scheduleRecovery(network, start)
            return
        }
        val chosen = recoveryNetwork
        if (chosen != null && chosen != network && networkCandidate(chosen).usable) return
        recoveryNetwork = network
        if (!recoveryNativeStopped || recoveryScheduled || recovery.inFlight || gate.active != null) return
        val start = PhysicalNetworkRecovery.ready(recovery, recovery.generation, candidate, now) ?: return
        scheduleRecovery(network, start)
    }
    private fun beginPhysicalRecovery() {
        val nodeId = networkRecovery?.nodeId ?: activeVpnNodeId
        if (nodeId.isEmpty() || !PhysicalNetworkRecovery.recoveryAllowed(view.phase, networkRecovery != null) || stopping.get()) return
        // A physical recovery supersedes any pending held-gateway choice.
        invalidateHoldFailover()
        retention.transitionFor(PhysicalNetworkRecovery.WINDOW_MS)
        networkGeneration = maxOf(networkGeneration, networkRecovery?.generation ?: 0) + 1
        networkRecovery = PhysicalNetworkRecovery.begin(nodeId, networkGeneration - 1, SystemClock.elapsedRealtime())
        networkGeneration = networkRecovery!!.generation
        recoveryNetwork = physical?.takeIf { networkCandidate(it).usable }
        cancelScheduledRecovery()
        recoveryConnectAttempt = null
        recoveryNativeStopped = false
        main.removeCallbacks(recoveryExpiry)
        main.postDelayed(recoveryExpiry, PhysicalNetworkRecovery.WINDOW_MS)
        val child = native
        native = null
        activeRuntimeEpoch = 0
        gate.cancel()
        readinessProbe?.close()
        readinessProbe = null
        physical = null
        publish(view.copy(phase = "Reconnecting", wakeRecovery = null, channels = null, pendingNodeId = null, pendingSwitchId = null,
            pendingSwitchRevision = null, selectedNodeId = nodeId, error = null))
        val stopPhase = view.phase
        Thread({
            runCatching { child?.stop(ChildStopReason.NETWORK_RECOVERY, stopPhase) }
            main.post {
                recoveryNativeStopped = true
                if (!stopping.get() && networkRecovery != null) {
                    val available = recoveryNetwork ?: connectivity.allNetworks.firstOrNull { networkCandidate(it).usable }
                    available?.let(::handlePhysicalCandidate)
                }
            }
        }, "terlimo-network-recovery").start()
    }
    private fun cancelScheduledRecovery() {
        scheduledRecovery?.let(main::removeCallbacks)
        scheduledRecovery = null
        recoveryScheduled = false
    }
    private fun scheduleRecovery(network: Network, start: NetworkRecoveryStart) {
        cancelScheduledRecovery()
        networkRecovery = start.state
        recoveryScheduled = true
        val callback = object : Runnable {
            override fun run() {
                if (scheduledRecovery !== this) return
                scheduledRecovery = null
                recoveryScheduled = false
                val state = networkRecovery
                val startable = state != null && !stopping.get() && gate.active == null &&
                    recoveryNetwork == network && networkCandidate(network).usable
                when (PhysicalNetworkRecovery.scheduledStartOutcome(state, start.state.generation,
                    stopping.get(), activeAttempt = gate.active != null, usablePath = startable)) {
                    RecoveryStartOutcome.START -> if (state != null) {
                        networkRecovery = PhysicalNetworkRecovery.admit(state)
                        recoveryNativeStopped = false
                        submitControl { begin("", network, state.generation) }
                    }
                    RecoveryStartOutcome.AWAIT -> if (state != null) {
                        // The delayed attempt was cancelled (candidate gone or the window passed
                        // first): never leave the recovery without retry or wait.
                        awaitRecoveryNetwork(state)
                        val available = recoveryNetwork ?: connectivity.allNetworks.firstOrNull { networkCandidate(it).usable }
                        available?.let(::handlePhysicalCandidate)
                    }
                    RecoveryStartOutcome.DISCARD -> Unit
                }
            }
        }
        scheduledRecovery = callback
        main.postDelayed(callback, start.delayMs)
    }
    private fun handleAttemptTerminal(attempt: String, code: String) {
        val recovery = networkRecovery
        when (TerminalFailurePolicy.outcome(code, recovery != null, activeVpnConfig != null)) {
            TerminalFailureOutcome.STOP -> stopAttempt(code)
            TerminalFailureOutcome.HOLD -> {
                completeCatalogRefresh(catalogGate.planTerminal(attempt, code))
                holdKillSwitch(code)
            }
            TerminalFailureOutcome.RETRY_RECOVERY -> {
                completeCatalogRefresh(catalogGate.planTerminal(attempt, code))
                recovery?.let { handleRecoveryTerminal(attempt, it.generation, code) }
            }
        }
    }
    private fun handleRecoveryTerminal(attempt: String?, generation: Long, code: String) {
        val current = networkRecovery
        if (current == null || current.generation != generation || stopping.get()) return
        val next = PhysicalNetworkRecovery.retry(current.copy(inFlight = false), SystemClock.elapsedRealtime())
        val network = recoveryNetwork
        val usableNetwork = network?.takeIf { networkCandidate(it).usable }
        if (next == null && usableNetwork != null) {
            // The bounded series is over while a usable physical path still exists: the server
            // or transport is unreachable, so hold the protected TUN as before.
            holdKillSwitch(code)
            return
        }
        // Either a bounded retry starts, or the physical path is gone and the same node must be
        // resumed automatically once a usable network returns (awaitingNetwork).
        val child = native
        android.util.Log.w("WDTT/Teardown", "stage=clear_native site=recovery_terminal child=" + (child != null))
        native = null
        if (attempt == null || gate.active == attempt) gate.cancel()
        readinessProbe?.close()
        readinessProbe = null
        cancelScheduledRecovery()
        recoveryNativeStopped = false
        recoveryConnectAttempt = null
        val stopPhase = view.phase
        Thread({
            runCatching { child?.stop(ChildStopReason.RECOVERY_RETRY, stopPhase) }
            main.post {
                val latest = networkRecovery
                recoveryNativeStopped = true
                if (latest == null || latest.generation != generation || stopping.get()) return@post
                if (next != null && usableNetwork != null) scheduleRecovery(usableNetwork, next)
                else {
                    awaitRecoveryNetwork(latest)
                    // A usable network may have returned while the child was stopping; do not
                    // wait for the next callback to observe it.
                    val available = recoveryNetwork ?: connectivity.allNetworks.firstOrNull { networkCandidate(it).usable }
                    available?.let(::handlePhysicalCandidate)
                }
            }
        }, "terlimo-network-recovery-retry").start()
    }
    /**
     * No usable physical path: keep the protected TUN and wait for a network callback. The
     * waiting state is bounded by the lifetime of this recovery state (Disconnect, lease expiry
     * or holdKillSwitch clears it); nothing retries until a usable network actually returns.
     */
    private fun awaitRecoveryNetwork(current: NetworkRecoveryState) {
        networkRecovery = PhysicalNetworkRecovery.await(current)
        publish(view.copy(phase = "Reconnecting", attempt = null, wakeRecovery = null, channels = null, error = null))
    }
    private fun holdKillSwitch(code: String) {
        if (stopping.get()) {
            clearHoldFailover()
            return
        }
        if (networkRecovery == null && view.phase == "KillSwitch") {
            // The hold is already active: keep it and surface the latest reason (e.g. a held
            // failover attempt failed before it could publish). An in-flight stop wait and the
            // armed choice are preserved; only Disconnect/expiry/new hold cancel them.
            HoldRetention.alreadyHeld(view, code)?.let { publish(it) }
            return
        }
        invalidateHoldFailover()
        android.util.Log.w("WDTT/Terminal", FailureDiagnostics.line("kill_switch", view.phase, gate.active, code, null))
        main.removeCallbacks(recoveryExpiry)
        main.removeCallbacks(expiry)
        leaseAlarms.cancel(0)
        networkRecovery = null
        recoveryNetwork = null
        cancelScheduledRecovery()
        recoveryNativeStopped = false
        val child = native
        android.util.Log.w("WDTT/Teardown", "stage=clear_native site=hold child=" + (child != null))
        native = null
        activeRuntimeEpoch = 0
        gate.cancel()
        readinessProbe?.close()
        readinessProbe = null
        publish(view.copy(phase = "KillSwitch", attempt = null, wakeRecovery = null, channels = null, pendingNodeId = null, pendingSwitchId = null,
            pendingSwitchRevision = null, error = code,
            accountAccess = view.accountAccess?.copy(current = false)))
        val stopPhase = view.phase
        holdStopGeneration++
        val stopGeneration = holdStopGeneration
        Thread({
            // The confirmed stop comes from the production NativeProcess contract (factual reap
            // within the existing bounded window); no child in this service means nothing to
            // confirm. A failed/unconfirmed stop never triggers an automatic retry.
            val stopped = child?.stop(ChildStopReason.KILL_SWITCH_HOLD, stopPhase) ?: true
            main.post {
                // Only the current stop generation may update the global stop readiness or the
                // hold state; a stale completion (new hold/recovery/Disconnect) changes nothing.
                if (!HoldFailoverPolicy.completionApplies(stopGeneration, holdStopGeneration, stopping.get())) {
                    return@post
                }
                recoveryNativeStopped = stopped
                completeHoldFailover(stopGeneration, stopConfirmed = stopped)
            }
        }, "terlimo-kill-switch-hold").start()
    }
    private fun persistCatalogCache(state: ViewState) {
        if (state.nodes.isEmpty()) return
        storage.writeCatalogCache(CatalogCacheCodec.encode(
            RetainedCatalog(state.nodes, state.selectedNodeId, state.catalogRevision)))
    }

    /**
     * One-tap Connect from the retained disconnected state. The retained id is applied
     * only after the freshly verified catalog, and only drives native select/admission;
     * Connected is never taken from cache. Fenced to the active attempt.
     */
    private fun autoConnectRetained(attempt: String, state: ViewState) {
        val target = connectOnCatalog ?: return
        if (gate.active != attempt || stopping.get()) return
        if (state.phase != "CatalogReady" || state.pendingNodeId != null) return
        if (NodeSelection.connectableNodeId(state.nodes, state.selectedNodeId) != target) {
            connectOnCatalog = null
            return
        }
        if (VpnService.prepare(this) != null) { connectOnCatalog = null; return }
        val elapsed = SystemClock.elapsedRealtime()
        if (!connectDeadline.start(attempt, elapsed)) { connectOnCatalog = null; return }
        explicitConnect.arm(ExplicitConnectGate.Entry.ONE_TAP_CONNECT, attempt)
        connectOnCatalog = null
        retention.loadForConnection()
        retention.transitionFor(ConnectDeadline.MILLIS)
        main.postDelayed({
            gate.ifActive(attempt) {
                if (connectDeadline.expired(attempt, SystemClock.elapsedRealtime()))
                    handleAttemptTerminal(attempt, "VPN_SETUP_TIMEOUT")
            }
        }, ConnectDeadline.MILLIS)
        submitControl {
            if (gate.active == attempt && view.phase == "CatalogReady" && view.pendingNodeId == null &&
                view.nodes.any { it.id == target })
                send(JSONObject().put("type", "select_node").put("node_id", target)
                    .put("explicit_connect", true))
        }
    }

    /**
     * The explicit-connect command with the optional host `gateway_key` (the selected browsed
     * gateway id). Without a selection the key is omitted and the accepted behavior applies.
     */
    private fun explicitConnectCommand(gatewayKey: String): JSONObject =
        JSONObject().put("type", "explicit_connect").also { message ->
            if (gatewayKey.isNotEmpty()) message.put("gateway_key", gatewayKey)
        }

    private fun armTrafficTick() {
        if (!stopping.get() && view.phase == "Connected") {
            main.removeCallbacks(trafficTick)
            main.postDelayed(trafficTick, TRAFFIC_TICK_MILLIS)
        }
    }

    private fun stopTrafficTick() = main.removeCallbacks(trafficTick)

    private fun stopAttempt(code: String?, caller: String = "unspecified") {
        android.util.Log.w("WDTT/Teardown", "stage=stop_enter child=" + (native != null) + " caller=" + caller)
        trialGate.reset()
        purchaseGate.reset()
        devicesGate.reset()
        loginGate.reset()
        // Transport teardown fences the old native stream: any outstanding purchase request
        // is dropped here, and its late callbacks are excluded by the attempt gate.
        purchaseFlight.reset()
        // Attempt teardown: no pending create result may correlate into a dead attempt.
        paymentCreates.clear()
        if (!stopping.compareAndSet(false, true)) {
            android.util.Log.w("WDTT/Teardown", "stage=stop_cas_fail")
            return
        }
        // Ownership of the teardown is established: no in-flight devices token may survive
        // its attempt (a stuck token would block every later send). A previously published
        // refusal/timeout/transport/malformed error is preserved.
        val releasedDevices = DevicesPolicy.releaseInFlight(view.devices, "SERVICE_UNAVAILABLE")
        if (releasedDevices != view.devices) publish(view.copy(devices = releasedDevices))
        connectOnCatalog = null
        invalidateHoldFailover()
        // Reset the per-attempt state and fence all older callbacks; a stale stop of a foreign
        // attempt can neither clear the timer nor reset the current state.
        completeCatalogRefresh(catalogGate.planTerminal(gate.active ?: "", code ?: "stopped"))
        if (mobileCatalog.onAttemptStop(gate.active) == MobileCatalogAction.DISARM) catalogTimer.clear()
        if (code != null) {
            android.util.Log.w("WDTT/Terminal", FailureDiagnostics.line("stop", view.phase, gate.active, code, null))
        }
        main.post { retention.close() }
        sleepPaused = false
        sleepResumeRequested = false
        retiringActors.incrementAndGet()
        activeRuntimeEpoch = 0
        gate.cancel()
        connectDeadline.clear()
        explicitConnect.clear()
        preAdmissionConnect = false
        preAdmissionGatewayKey = ""
        actor.close()
        signer.shutdownNow()
        captcha = null
        main.removeCallbacks(expiry)
        main.removeCallbacks(recoveryExpiry)
        main.removeCallbacks(accessTick)
        stopTrafficTick()
        stopUsageTick()
        val trafficOff = trafficSampler.onDisconnect()
        trafficReadGate.bump()
        val usageOff = view.serverUsage
        publish(view.copy(traffic = trafficOff,
            serverUsage = usageOff?.copy(stale = true),
            serverUsageUnavailable = usageOff == null))
        networkRecovery = null
        recoveryNetwork = null
        cancelScheduledRecovery()
        expiresElapsed = 0
        leaseAlarms.close()
        readinessProbe?.close()
        publish(view.copy(phase = "Stopping", attempt = null, wakeRecovery = null, channels = null, error = code))
        val stopPhase = view.phase
        // Exactly one teardown thread per Service; not an actor item and not behind its backlog.
        Thread({
            val child = native; native = null
            android.util.Log.w("WDTT/Teardown", "stage=teardown_thread child=" + (child != null))
            val stopped = runCatching { child?.stop(ChildStopReason.TEARDOWN, stopPhase) }.getOrNull() ?: (child == null)
            android.util.Log.w("WDTT/Teardown", "stage=teardown_child stopped=" + stopped)
            main.post {
                networkCallback?.let { runCatching { connectivity.unregisterNetworkCallback(it) } }
                networkCallback = null
            }
            vpnWorker.execute {
                android.util.Log.w("WDTT/Teardown", "stage=vpn_worker_enter")
                val currentBackend = backend
                val oldBackend = rollbackBackend
                val cleanCurrent = runCatching { currentBackend?.setState(tunnel, Tunnel.State.DOWN, null) }.isSuccess
                val cleanOld = oldBackend === currentBackend || runCatching { oldBackend?.retireState(tunnel) }.isSuccess
                val clean = cleanCurrent && cleanOld
                activeVpnConfig = null
                activeRoutingPlan = null
                activeVpnNodeId = ""
                // The native child was stopped above; when WireGuard teardown succeeded the tunnel
                // is actually gone, so the "native stopped" marker must not keep the routing editor
                // locked until onDestroy. On failure it stays set (phase becomes Error) and save is
                // conservatively denied rather than claiming a proven DOWN.
                if (clean) recoveryNativeStopped = true
                rollbackVpnConfig = null
                rollbackRoutingPlan = null
                rollbackVpnNodeId = ""
                rollbackBackend = null
                rollbackExpiresElapsed = 0
                rollbackLeaseEnd = ""
                // Native and VPN are already stopped; do not publish restart-ready while
                // an old AtomicFile write may still finish. Other Service instances also wait.
                while (!actor.awaitStopped(250)) { /* never runs on main/reader/actor */ }
                // Freeze one completed attempt before a successor may start. UI must
                // never combine independently updated maps from different attempts.
                completedDiagnostics = UserDiagnostics.describe(lastBootstrap, lastReadiness, lastRelay, lastVpnDiagnostic)
                retiringActors.decrementAndGet()
                // Keep the last verified catalog/selection for the next Connect.
                publish(SessionRetention.onStop(view, if (code == null && clean) "Idle" else "Error",
                    if (clean) code else "CLEANUP_FAILED"))
                android.util.Log.w("WDTT/Teardown", "stage=teardown_complete")
                main.post { stopForeground(STOP_FOREGROUND_REMOVE); stopSelf() }
            }
            vpnWorker.shutdown()
        }, "terlimo-stop").start()
    }
    override fun onDestroy() {
        if (runningService === this) runningService = null
        scheduleListener?.let { CatalogRefreshScheduleState.removeScheduleListener(it) }
        scheduleListener = null
        retention.close()
        listeners.remove(statusListener)
        stopAttempt(null, "on_destroy")
        captcha = null
        main.removeCallbacksAndMessages(null)
        leaseAlarms.close()
        networkCallback?.let { runCatching { connectivity.unregisterNetworkCallback(it) } }
        networkCallback = null
        readinessProbe?.close()
        if (lifecycleRegistered) {
            unregisterReceiver(lifecycleReceiver)
            lifecycleRegistered = false
        }
        super.onDestroy()
    }
    companion object {
        @Volatile internal var completedDiagnostics: String? = null
            private set
        @Volatile internal var lastReadiness: Map<String, Any> = emptyMap()
        @Volatile internal var lastRelay: Map<String, Long> = emptyMap()
        @Volatile internal var lastVpnDiagnostic: Map<String, String> = emptyMap()
        @Volatile internal var lastBootstrap: Map<String, Any> = emptyMap()
        @Volatile internal var lastPhysicalNetwork: Map<String, Boolean> = emptyMap()
        private val RELAY_FIELDS = setOf("local_udp_rx_packets", "local_udp_rx_bytes", "relay_rx_packets", "relay_rx_bytes", "relay_rx_92_packets",
            "local_udp_write_packets", "local_udp_write_bytes", "local_udp_write_errors", "local_udp_write_92_packets",
            "config_worker_start_count", "config_worker_ready_count", "data_worker_start_count", "data_worker_ready_count",
            "data_dial_success_count", "data_dial_failure_count", "data_auth_success_count", "data_auth_failure_count",
            "data_session_failure_count", "data_canceled_count", "data_error_transport_timeout_count", "data_error_proof_invalid_count",
            "data_error_session_not_ready_count", "data_error_lease_conflict_count", "data_error_auth_required_count", "data_error_other_count")
        private val VPN_DIAGNOSTIC_FIELDS = setOf("vpn_stage", "vpn_error_class", "vpn_auth_code")
        private val retiringActors = java.util.concurrent.atomic.AtomicInteger(0)
        private val viewLock = Any()
        @Volatile internal var view = ViewState()
            private set
        @Volatile private var runningService: SessionService? = null

        /**
         * Consistent read-only snapshot of the authoritative tunnel state for the routing editor.
         * Combines the published phase with the actual applied/applying tunnel markers, because a
         * `CatalogReady` phase can coexist with a retained recovery/sleep tunnel that is still UP.
         */
        internal fun routingEditSnapshot(): RoutingEditSnapshot {
            val service = runningService
            val tunnel = if (service == null) {
                TunnelApplicationState.NONE
            } else {
                RoutingEditState.tunnelOf(
                    activeVpnConfig = service.activeVpnConfig != null,
                    sleepPaused = service.sleepPaused,
                    networkRecovery = service.networkRecovery != null,
                    nativeStopped = service.recoveryNativeStopped,
                    applying = service.vpnApplying.get(),
                )
            }
            return RoutingEditSnapshot(view.phase, tunnel)
        }
        internal val listeners = CopyOnWriteArraySet<(ViewState) -> Unit>()
        @Volatile internal var captcha: CaptchaPrompt? = null
        private val NATIVE_PHASES = setOf("ImportVerified", "BootstrapConnecting", "Registering", "ResolvingOperation", "SyncingAccess", "CatalogReady", "NodeAuthenticating", "WaitingUser")
        private fun publish(state: ViewState) {
            synchronized(viewLock) { view = state }
            listeners.forEach { it(state) }
        }
        internal fun replaceSubscriptionIfUnchanged(expected: ViewState, mutation: () -> Unit): Boolean {
            val replacement = synchronized(viewLock) {
                if (view !== expected || view.phase !in setOf("Idle", "Error")) return false
                mutation()
                ViewState().also { view = it }
            }
            listeners.forEach { it(replacement) }
            return true
        }
        private fun safeCode(value: String?) = value?.takeIf { it.matches(Regex("[A-Z][A-Z0-9_]{1,63}")) } ?: "HOST_ERROR"
    }
}

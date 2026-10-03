package xyz.terlimo.test

import android.Manifest
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.res.ColorStateList
import android.content.Intent
import android.widget.CheckBox
import android.content.pm.PackageManager
import android.net.VpnService
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.provider.Settings
import android.view.View
import android.widget.*

/** Product UI; no admin/deploy controls or secret diagnostics. */
class MainActivity : Activity() {
    private lateinit var appUpdateUi: AppUpdateUi
    override fun attachBaseContext(newBase: android.content.Context) {
        // §26.1: resolve the stored per-user theme before any view is created.
        super.attachBaseContext(AppTheme.wrap(newBase))
    }

    private lateinit var status: TextView
    private lateinit var nodes: Spinner
    private lateinit var connectButton: Button
    private lateinit var activateHourButton: Button
    private lateinit var registerTelegramButton: Button
    private lateinit var loginTelegramButton: Button
    private lateinit var trialButton: Button
    private lateinit var homeContextButton: Button
    private lateinit var referralUi: ReferralUi
    private lateinit var accountStatus: TextView
    private lateinit var homeAnnouncements: LinearLayout
    private lateinit var recoveryErrorButton: Button
    private lateinit var captchaButton: Button
    private lateinit var subscription: TextView
    private lateinit var subscriptionTerm: TextView
    private lateinit var subscriptionDevices: TextView
    private lateinit var subscriptionTraffic: TextView
    private lateinit var subscriptionPlan: TextView
    private lateinit var batteryMessage: TextView
    private lateinit var batteryButton: Button
    private lateinit var scheduleStatus: TextView
    private var catalogScheduleRefused = false
    private lateinit var devicesBlock: LinearLayout
    private lateinit var devicesRefreshButton: Button
    private lateinit var devicesCount: TextView
    private lateinit var purchasePlansButton: Button
    private lateinit var purchasePlanButton: Button
    private lateinit var purchaseMethodButton: Button
    private lateinit var purchaseQuoteLine: TextView
    private lateinit var purchasePayButton: Button
    private lateinit var purchaseContinueButton: Button
    private lateinit var purchaseCheckButton: Button
    private lateinit var purchaseStatus: TextView
    private lateinit var purchaseReference: TextView
    private var purchasePlanId: String? = null
    private var purchaseMethod: String? = null
    private var purchaseRenewExtraSlotIds: List<String> = emptyList()
    // An explicit new selection cannot reuse the previously displayed quote, even before
    // the service consumes its queued command. Recreation restores neither selection nor Pay.
    private var purchaseRejectedQuoteId: String? = null
    private var displayedPurchaseQuote: PaymentQuote? = null
    // §3.2B: explicit-action checkout-open state (opened id, live attempt key, visible error).
    // Owned by CheckoutOpenPolicy; only an explicitly armed tap can open, and renders carry the
    // current attempt key so a refresh/replaced attempt/recreation can never auto-open.
    private val checkoutOpenPolicy = CheckoutOpenPolicy()
    private lateinit var catalogView: ServerCatalogView
    private lateinit var orbitHeader: OrbitHomeHeader
    private val installationStore by lazy { InstallationStore(this) }
    private var labels = emptyList<NodeLabel>()
    private var pendingChoiceId: String? = null
    private var selectedForConsent: String? = null
    private var preAdmissionConsentRequest = false
    private var consentDenied = false
    /** §26.2 launch token: one auto-connect per user open; consumed by Disconnect/Off/denial. */
    /** §26.2 unified launch token: arm -> (optional consent) -> send, recreation-stable. */
    private val autoConnectLaunch = AutoConnectLaunchToken()
    private var autoConnectToggle: CheckBox? = null
    // S5 §11 announcements display (Help tab). Values are rendered from ViewState only.
    private lateinit var announcementsBadge: TextView
    private lateinit var announcementsDenied: TextView
    private lateinit var announcementsEmpty: TextView
    private lateinit var announcementsList: LinearLayout
    private var openHelp: (() -> Unit)? = null
    private var helpTabButton: Button? = null
    private var openTab: ((NavTarget) -> Unit)? = null
    // §26.1: survive a theme-driven recreate without repeating side effects.
    private var visibleTarget: NavTarget = NavTarget.HOME
    private var restoreTarget: NavTarget? = null
    private var lastRenderedState: ViewState? = null
    private val listener: (ViewState) -> Unit = { state -> runOnUiThread { render(state) } }
    private val main = Handler(Looper.getMainLooper())
    /** Activity started state of the display-only countdown; cleared in onStop. */
    private var statusTickStarted = false
    /**
     * Display-only countdown refresh for the visible status texts. It reuses the frozen-clock
     * policy on the existing render surfaces only; it never starts an attempt, talks to native
     * or changes admission. A late callback after onStop sees statusTickStarted == false and
     * cannot re-arm, so nothing polls while the screen is gone.
     */
    private val statusTick = object : Runnable {
        override fun run() {
            if (!statusTickStarted) return
            refreshStatusTexts()
            if (AccountAccessDisplayRefresh.shouldPost(statusTickStarted, SessionService.view.accountAccess,
                    android.os.SystemClock.elapsedRealtime()))
                main.postDelayed(this, AccountAccessDisplayRefresh.TICK_MILLIS)
        }
    }


    override fun onCreate(savedInstanceState: Bundle?) {
        setTheme(AppTheme.platformTheme())
        super.onCreate(savedInstanceState)
        appUpdateUi = AppUpdateUi(this)
        SessionService.restorePurchaseHint(installationStore)
        SessionService.restoreReferralHint(installationStore)
        // §26.5: restore the persisted schedule and reconcile BEFORE building the Settings UI,
        // so the radio group shows the real stored mode. Idempotent: a recreation never
        // restarts the period.
        CatalogRefreshScheduleState.restore(this)
        // §26.5: the STARTUP reconcile result is honoured exactly like a later change, so a
        // refused schedule is never shown as active; a scheduler throw is honestly reported.
        val startupReconcile = runCatching { CatalogRefreshScheduler.reconcile(this) }.getOrNull()
        catalogScheduleRefused = if (startupReconcile == null) {
            CatalogRefreshScheduleState.mode() != CatalogRefreshMode.OFF
        } else {
            CatalogRefreshPolicy.refused(
                CatalogRefreshScheduleState.mode(), startupReconcile.decision, startupReconcile.scheduleResult)
        }
        // Android 13+ hides the foreground-service notification without the runtime
        // permission. Ask at most once per install; the tunnel never depends on the answer
        // and the in-app status/diagnostics remain when the user declines.
        val permissionPrefs = getSharedPreferences(NotificationPermission.PREFS, MODE_PRIVATE)
        val permissionAsked = permissionPrefs.getBoolean(NotificationPermission.ASKED_KEY, false)
        if (NotificationPermission.shouldRequest(Build.VERSION.SDK_INT,
                checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED,
                permissionAsked)) {
            permissionPrefs.edit().putBoolean(NotificationPermission.ASKED_KEY, true).apply()
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), NotificationPermission.REQUEST_CODE)
        }
        // §26.2 launch token: a fresh Activity (savedInstanceState == null) arms one token;
        // recreation and consent returns restore the consumed/pending status and never revive it.
        // §26.2: a genuine cold user launch arms one token; recreation, a consent return or
        // an external deep-link launch never does. The pending consent generation survives
        // recreation exactly like the existing checkout markers.
        autoConnectLaunch.restore(
            savedInstanceState?.getLong(STATE_AUTOCONNECT_GENERATION, 0L) ?: 0L,
            savedInstanceState?.getBoolean(STATE_AUTOCONNECT_CONSENT_PENDING, false) == true,
        )
        val autoConnectUserLaunch = savedInstanceState == null && isUserLaunchIntent(intent)
        restoreTarget = savedInstanceState?.getString(STATE_VISIBLE_TAB)?.let {
            runCatching { NavTarget.valueOf(it) }.getOrNull()
        }
        // Restores only the opened id and visible error; never a live open marker, so a
        // recreated Activity cannot auto-open the browser (explicit action only).
        checkoutOpenPolicy.restoreFrom(savedInstanceState)
        val mainPanel = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
        }
        val subscriptionPanel = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(32, 24, 32, 24)
            visibility = View.GONE
        }
        val helpPanel = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(32, 24, 32, 24)
            visibility = View.GONE
            addView(TextView(this@MainActivity).apply { text = "Помощь"; textSize = 24f })
            addView(TextView(this@MainActivity).apply {
                text = "Диагностика проверяет VPN, интернет, DNS и доступность соединения. Отчёт не содержит ключей и кода восстановления."
                setPadding(0, 12, 0, 16)
            })
            // §27/07.6: runtime version, short instructions and the confirmed public
            // TERLIMO contacts/legal pages. Links open the existing external app.
            addView(TextView(this@MainActivity).apply {
                text = HelpContent.versionText(this@MainActivity)
                setPadding(0, 0, 0, 12)
            })
            addView(TextView(this@MainActivity).apply {
                text = HelpContent.CONNECT_TITLE; textSize = 18f; setPadding(0, 8, 0, 0)
            })
            addView(TextView(this@MainActivity).apply { text = HelpContent.CONNECT_TEXT; setPadding(0, 4, 0, 0) })
            addView(TextView(this@MainActivity).apply {
                text = HelpContent.SPEED_TITLE; textSize = 18f; setPadding(0, 12, 0, 0)
            })
            addView(TextView(this@MainActivity).apply { text = HelpContent.SPEED_TEXT; setPadding(0, 4, 0, 0) })
            addView(TextView(this@MainActivity).apply {
                text = HelpContent.NETWORK_TITLE; textSize = 18f; setPadding(0, 12, 0, 0)
            })
            addView(TextView(this@MainActivity).apply { text = HelpContent.NETWORK_TEXT; setPadding(0, 4, 0, 0) })
            addView(TextView(this@MainActivity).apply {
                text = HelpContent.CONTACTS_TITLE; textSize = 20f; setPadding(0, 20, 0, 0)
            })
            addView(helpLink(HelpContent.BOT_LABEL, HelpContent.BOT_URL))
            addView(helpLink(HelpContent.CHANNEL_LABEL, HelpContent.CHANNEL_URL))
            addView(helpLink(HelpContent.SUPPORT_GROUP_LABEL, HelpContent.SUPPORT_GROUP_URL))
            addView(helpLink(HelpContent.SUPPORT_LABEL, HelpContent.SUPPORT_URL))
            addView(helpLink(HelpContent.SITE_LABEL, HelpContent.SITE_URL))
            addView(helpLink(HelpContent.PRIVACY_LABEL, HelpContent.PRIVACY_URL))
            addView(helpLink(HelpContent.AGREEMENT_LABEL, HelpContent.AGREEMENT_URL))
            // S5 §11 «Уведомления»: one-way service messages only, never a chat. The red dot
            // reflects the server unread state after local read acknowledgements.
            announcementsBadge = TextView(this@MainActivity).apply { textSize = 20f }
            announcementsDenied = TextView(this@MainActivity).apply { setPadding(0, 4, 0, 4) }
            announcementsEmpty = TextView(this@MainActivity).apply {
                text = AnnouncementsText.EMPTY
                setPadding(0, 8, 0, 8)
            }
            announcementsList = LinearLayout(this@MainActivity).apply { orientation = LinearLayout.VERTICAL }
            addView(LinearLayout(this@MainActivity).apply {
                orientation = LinearLayout.HORIZONTAL
                addView(TextView(this@MainActivity).apply { text = "Новости и уведомления"; textSize = 20f },
                    LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f))
                addView(announcementsBadge)
            })
            addView(announcementsDenied)
            addView(announcementsEmpty)
            addView(announcementsList)
        }
        subscriptionPanel.addView(TextView(this).apply { text = "Подписка TERLIMO"; textSize = 24f })
        subscription = TextView(this).apply { setPadding(0, 16, 0, 16); visibility = View.VISIBLE }
        subscriptionPanel.addView(subscription)
        subscriptionTerm = TextView(this).apply { setPadding(0, 0, 0, 8); visibility = View.GONE }
        subscriptionPanel.addView(subscriptionTerm)
        subscriptionDevices = TextView(this).apply { setPadding(0, 0, 0, 8); visibility = View.GONE }
        subscriptionPanel.addView(subscriptionDevices)
        subscriptionTraffic = TextView(this).apply { setPadding(0, 0, 0, 8); visibility = View.GONE }
        subscriptionPanel.addView(subscriptionTraffic)
        subscriptionPlan = TextView(this).apply { setPadding(0, 0, 0, 8); visibility = View.GONE }
        subscriptionPanel.addView(subscriptionPlan)
        // §§18–19 connected devices on the existing subscription surface.
        devicesRefreshButton = Button(this).apply {
            text = "Обновить устройства"; visibility = View.GONE; minHeight = dp(48)
            setOnClickListener {
                startForegroundService(Intent(this@MainActivity, SessionService::class.java)
                    .setAction("devices_refresh"))
            }
        }
        subscriptionPanel.addView(devicesRefreshButton)
        devicesCount = TextView(this).apply { setPadding(0, 0, 0, 8); visibility = View.GONE }
        subscriptionPanel.addView(devicesCount)
        devicesBlock = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        subscriptionPanel.addView(devicesBlock)
        // S5 purchase/renewal lives on this existing "Подписка" surface; no second tab.
        subscriptionPanel.addView(TextView(this).apply { text = "Оплата и продление"; textSize = 18f })
        purchasePlansButton = Button(this).apply {
            text = "Показать тарифы"
            setOnClickListener {
                startForegroundService(Intent(this@MainActivity, SessionService::class.java).setAction("purchase_plans"))
            }
        }
        subscriptionPanel.addView(purchasePlansButton)
        purchasePlanButton = Button(this).apply {
            visibility = View.GONE
            contentDescription = "Тариф подписки"
            setOnClickListener { showPurchasePlans() }
        }
        subscriptionPanel.addView(purchasePlanButton)
        purchaseMethodButton = Button(this).apply {
            visibility = View.GONE
            contentDescription = "Способ оплаты"
            setOnClickListener { showPurchaseMethods() }
        }
        subscriptionPanel.addView(purchaseMethodButton)
        purchaseQuoteLine = TextView(this).apply { visibility = View.GONE }
        subscriptionPanel.addView(purchaseQuoteLine)
        purchasePayButton = Button(this).apply {
            text = PaymentsText.PAY_TEXT
            visibility = View.GONE
            setOnClickListener {
                val quote = selectedPurchaseQuote(SessionService.view.purchase)
                if (quote == null || quote != displayedPurchaseQuote || purchaseBusy()) {
                    render(SessionService.view)
                    return@setOnClickListener
                }
                noteExplicitPay(quote.quoteId)
                startForegroundService(Intent(this@MainActivity, SessionService::class.java)
                    .setAction("purchase_pay").putExtra("quote_id", quote.quoteId))
            }
        }
        subscriptionPanel.addView(purchasePayButton)
        purchaseContinueButton = Button(this).apply {
            text = PaymentsText.CONTINUE_PAYMENT_TEXT
            visibility = View.GONE
            setOnClickListener { continueExistingPayment() }
        }
        subscriptionPanel.addView(purchaseContinueButton)
        purchaseCheckButton = Button(this).apply {
            text = "Проверить статус оплаты"
            visibility = View.GONE
            setOnClickListener {
                startForegroundService(Intent(this@MainActivity, SessionService::class.java).setAction("purchase_check"))
            }
        }
        subscriptionPanel.addView(purchaseCheckButton)
        purchaseStatus = TextView(this).apply { setPadding(0, 8, 0, 8); visibility = View.VISIBLE }
        subscriptionPanel.addView(purchaseStatus)
        purchaseReference = TextView(this).apply { visibility = View.GONE }
        subscriptionPanel.addView(purchaseReference)
        orbitHeader = OrbitHomeHeader(this,
            onPower = {
                val state = SessionService.view
                val projected = projectedState()
                PowerDiagnostics.line("onPower",
                    "live" to state.phase, "proj" to projected.phase,
                    "display" to projected.displayMode, "nodes" to projected.nodes.size.toString(),
                    "selPresent" to (projected.selectedNodeId.isNotEmpty()).toString(),
                    "selValid" to (NodeSelection.displayedNodeId(projected.nodes, projected.selectedNodeId) != null).toString(),
                    "pendChoice" to (pendingChoiceId != null).toString(),
                    "pendNode" to (projected.pendingNodeId != null).toString(),
                    "attempt" to (!projected.attempt.isNullOrEmpty()).toString())
                if (state.recoveryStatus == "RECOVERY_RUNNING") {
                    cancelAutoConnectLaunch()
                    selectedForConsent = null
                    startService(Intent(this, SessionService::class.java).setAction("cancel"))
                } else if (PreAdmissionConnect.connectable(state, pendingChoiceId != null)) {
                    PowerDiagnostics.line("onPower.branch", "value" to "preAdmission")
                    requestPreAdmissionConsent(state)
                } else if (state.phase in setOf("Connected", "Starting", "BootstrapConnecting", "NodeAuthenticating", "ConfiguringVPN", "SwitchingServer", "WaitingUser", "Reconnecting", "SleepPaused", "KillSwitch")) {
                    PowerDiagnostics.line("onPower.branch", "value" to "cancel", "phase" to state.phase)
                    cancelAutoConnectLaunch()
                    selectedForConsent = null
                    startService(Intent(this, SessionService::class.java).setAction("cancel"))
                } else {
                    PowerDiagnostics.line("onPower.branch", "value" to "requestConsent", "phase" to state.phase)
                    requestVpnConsentForCurrentSelection()
                }
            },
            onRefresh = {
                if (SessionService.view.recoveryStatus != "RECOVERY_RUNNING")
                    startForegroundService(Intent(this, SessionService::class.java).setAction("refresh"))
            },
        )
        mainPanel.addView(orbitHeader)
        homeContextButton = Button(this).apply {
            visibility = View.GONE
            setOnClickListener {
                when (HomeAccessAction.forState(projectedState(), pendingChoiceId != null)) {
                    HomeAccessAction.HOUR -> activateHourButton.performClick()
                    HomeAccessAction.REGISTER -> openTab?.invoke(NavTarget.SUBSCRIPTION)
                    HomeAccessAction.TRIAL -> trialButton.performClick()
                    HomeAccessAction.PURCHASE -> openTab?.invoke(NavTarget.SUBSCRIPTION)
                    HomeAccessAction.NONE -> Unit
                }
            }
        }
        mainPanel.addView(homeContextButton)
        referralUi = ReferralUi(this,
            onCopy = { code ->
                (getSystemService(CLIPBOARD_SERVICE) as android.content.ClipboardManager)
                    .setPrimaryClip(android.content.ClipData.newPlainText("Приглашение TERLIMO", code))
                Toast.makeText(this, "Скопировано", Toast.LENGTH_SHORT).show()
            },
            onShare = { code ->
                startActivity(Intent.createChooser(Intent(Intent.ACTION_SEND).apply {
                    type = "text/plain"
                    putExtra(Intent.EXTRA_TEXT, code)
                }, "Поделиться приглашением"))
            },
            onDraftChanged = { draft ->
                runCatching { SessionService.saveReferralDraft(installationStore, draft) }
                    .onFailure { Toast.makeText(this, "Не удалось сохранить черновик", Toast.LENGTH_SHORT).show() }
            },
            onSubmit = {
                referralUi.incomingCodeDraft()?.let { draft ->
                    runCatching { SessionService.saveReferralDraft(installationStore, draft) }
                        .onSuccess { referralAction("referral_submit") }
                        .onFailure { Toast.makeText(this, "Код не отправлен: не удалось сохранить черновик", Toast.LENGTH_LONG).show() }
                }
            },
            onClear = {
                runCatching {
                    val saved = ReferralJournal(InstallationReferralStateStore(installationStore), installationStore.installationId()).state()
                    if (saved.candidate == null && !saved.unresolved) SessionService.saveReferralDraft(installationStore, "")
                    else referralAction("referral_clear")
                }.onFailure { Toast.makeText(this, "Не удалось очистить код", Toast.LENGTH_SHORT).show() }
            },
            onRetry = { referralAction("referral_retry") },
            onRefresh = { referralAction("referral_info") })
        mainPanel.addView(referralUi.invitationPanel())
        homeAnnouncements = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        mainPanel.addView(homeAnnouncements)
        val accountPanel = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            addView(TextView(this@MainActivity).apply { text = "Аккаунт и начало доступа"; textSize = 20f })
        }
        accountStatus = TextView(this)
        accountPanel.addView(accountStatus)
        subscriptionPanel.addView(accountPanel, 1)
        activateHourButton = Button(this).apply {
            text = "Активировать 1 час для регистрации"
            visibility = View.GONE
            setOnClickListener { requestPreAdmissionConsent(projectedState()) }
        }
        accountPanel.addView(activateHourButton)
        registerTelegramButton = Button(this).apply {
            text = "Зарегистрироваться в Telegram"
            visibility = View.GONE
            setOnClickListener {
                startForegroundService(Intent(this@MainActivity, SessionService::class.java)
                    .setAction("telegram_register"))
            }
        }
        accountPanel.addView(registerTelegramButton)
        loginTelegramButton = Button(this).apply {
            text = "Уже есть аккаунт? Войти через Telegram"
            visibility = View.GONE
            setOnClickListener {
                startForegroundService(Intent(this@MainActivity, SessionService::class.java)
                    .setAction("telegram_login"))
            }
        }
        accountPanel.addView(loginTelegramButton)
        trialButton = Button(this).apply {
            text = "Получить 7 дней"
            visibility = View.GONE
            setOnClickListener {
                startForegroundService(Intent(this@MainActivity, SessionService::class.java)
                    .setAction("trial_activate"))
            }
        }
        accountPanel.addView(trialButton)
        catalogView = ServerCatalogView(this,
            onRefresh = { startForegroundService(Intent(this, SessionService::class.java).setAction("refresh")) },
            onSelect = { requestNodeSelection(it) },
            onProbe = { nodeId -> startService(Intent(this, SessionService::class.java).setAction("probe").putExtra("node_id", nodeId)) },
            onProbeAll = { startService(Intent(this, SessionService::class.java).setAction("probe_all")) },
            onProbeAllCancel = {
                // Same idle-command gate as navigation: cancel only an actually running ping.
                if (SessionService.hasActiveCommonPing()) {
                    startService(Intent(this, SessionService::class.java)
                        .setAction(NavigationServiceCommands.CANCEL_COMMON_PING))
                }
            },
            showChrome = false,
            onSupport = { openHelpLink(HelpContent.SUPPORT_URL) },
        )
        mainPanel.addView(catalogView)
        mainPanel.addView(TextView(this).apply { text = "Доступные серверы:"; visibility = View.GONE })
        nodes = Spinner(this)
        nodes.visibility = View.GONE // retained only for the existing deterministic device harness
        nodes.contentDescription = "Выбор сервера из полученного каталога"
        nodes.onItemSelectedListener = object : AdapterView.OnItemSelectedListener {
            override fun onNothingSelected(parent: AdapterView<*>?) = Unit
            override fun onItemSelected(parent: AdapterView<*>?, view: View?, position: Int, id: Long) {
                val state = SessionService.view
                if (state.phase != "CatalogReady") return
                val chosen = NodeSelection.nodeIdAtSpinnerPosition(labels, position)
                val displayed = pendingChoiceId ?: state.pendingNodeId ?:
                    NodeSelection.displayedNodeId(state.nodes, state.selectedNodeId)
                if (chosen == null) {
                    if (displayed != null) nodes.setSelection(NodeSelection.spinnerPosition(labels, displayed), false)
                    return
                }
                if (chosen == displayed) return
                pendingChoiceId = chosen
                selectedForConsent = null
                render(state)
                startService(Intent(this@MainActivity, SessionService::class.java)
                    .setAction("choose").putExtra("node_id", chosen))
            }
        }
        mainPanel.addView(nodes)
        connectButton = Button(this).apply {
            text = "Подключить выбранный сервер"
            visibility = View.GONE // product action is the Orbit power button
            setOnClickListener {
                val state = SessionService.view
                val nodeId = NodeSelection.nodeIdAtSpinnerPosition(labels, nodes.selectedItemPosition)
                if (nodeId == null) {
                    requestPreAdmissionConsent(state)
                    return@setOnClickListener
                }
                if (pendingChoiceId != null || state.pendingNodeId != null ||
                    nodeId != NodeSelection.connectableNodeId(state.nodes, state.selectedNodeId)) return@setOnClickListener
                consentDenied = false
                selectedForConsent = nodeId
                val consent = VpnService.prepare(this@MainActivity)
                if (consent != null) startActivityForResult(consent, 100) else connectSelected()
            }
        }
        mainPanel.addView(connectButton)
        captchaButton = Button(this).apply {
            text = "Открыть CAPTCHA VK"
            setOnClickListener { ManlCaptchaWebViewManager.checkAndShowPendingCaptcha(this@MainActivity) }
        }
        mainPanel.addView(captchaButton)
        mainPanel.addView(Button(this).apply {
            text = "Отменить / отключить"
            visibility = View.GONE
            setOnClickListener {
                cancelAutoConnectLaunch()
                selectedForConsent = null
                startService(Intent(this@MainActivity, SessionService::class.java).setAction("cancel"))
            }
        })
        status = TextView(this).apply { textSize = 18f; setPadding(0, 24, 0, 0) }
        mainPanel.addView(status)
        recoveryErrorButton = Button(this).apply {
            text = "Восстановить подключение"
            setOnClickListener { openRecoveryEditor() }
        }
        mainPanel.addView(recoveryErrorButton)
        helpPanel.addView(Button(this).apply {
            text = "Диагностика последней попытки"
            setOnClickListener {
                val text = SessionService.completedDiagnostics
                    ?: "Завершённой попытки в этом процессе ещё нет. Это не архив; новое подключение для просмотра не запускается."
                val dialog = android.app.AlertDialog.Builder(this@MainActivity)
                    .setTitle("Безопасная диагностика").setMessage(text).setPositiveButton("Закрыть", null).create()
                dialog.show()
            }
        })
        val mainSurface = ScrollView(this).apply {
            isFillViewport = true
            setBackgroundColor(TerlimoCatalogBrandTokens.BACKGROUND.toInt())
            addView(mainPanel)
        }
        val subscriptionSurface = ScrollView(this).apply {
            isFillViewport = true
            setBackgroundColor(TerlimoCatalogBrandTokens.BACKGROUND.toInt())
            visibility = View.GONE
            subscriptionPanel.visibility = View.VISIBLE
            addView(subscriptionPanel)
        }
        val referralSurface = ScrollView(this).apply {
            isFillViewport = true
            visibility = View.GONE
            addView(referralUi.panel())
        }
        val helpSurface = ScrollView(this).apply {
            isFillViewport = true
            setBackgroundColor(TerlimoCatalogBrandTokens.BACKGROUND.toInt())
            visibility = View.GONE
            helpPanel.visibility = View.VISIBLE
            addView(helpPanel)
        }
        // Settings owns routing, recovery and the existing local settings.
        val settingsPanel = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(32, 32, 32, 48)
            addView(TextView(this@MainActivity).apply { text = "Настройки"; textSize = 24f })
            addView(Button(this@MainActivity).apply {
                text = "Маршрутизация"
                setOnClickListener {
                    // The parent surface remains Settings, including across recreation/Back.
                    openTab?.invoke(NavTarget.SETTINGS)
                    startActivity(Intent(this@MainActivity, RoutingSettingsActivity::class.java))
                }
            })
            addView(appUpdateUi.panel())
            addView(Button(this@MainActivity).apply {
                text = "Восстановить подключение"
                setOnClickListener { openRecoveryEditor() }
            })
            addView(TextView(this@MainActivity).apply {
                text = FirstReleaseLinks.RECOVERY_ENTRY_TEXT
            })
            addView(Button(this@MainActivity).apply {
                text = "Получить в Telegram"
                isEnabled = FirstReleaseLinks.recoveryBotUrl() != null
                setOnClickListener { FirstReleaseLinks.recoveryBotUrl()?.let(::openHelpLink) }
            })
            addView(Button(this@MainActivity).apply {
                text = "Получить на сайте"
                isEnabled = FirstReleaseLinks.recoverySiteUrl() != null
                setOnClickListener { FirstReleaseLinks.recoverySiteUrl()?.let(::openHelpLink) }
            })
            addView(helpLink("Открыть Telegram-бота", HelpContent.BOT_URL))
            addView(Button(this@MainActivity).apply {
                text = "Работа с выключенным экраном"
                minHeight = dp(48)
                setOnClickListener { startActivity(Intent(this@MainActivity, RetentionSettingsActivity::class.java)) }
            })
            // §26.4: honest battery-optimization warning with a system-settings entry. The
            // app changes nothing; the user chooses "Без ограничений / Не оптимизировать".
            addView(TextView(this@MainActivity).apply {
                text = "Энергосбережение"
                textSize = 18f
                setPadding(0, dp(20), 0, 0)
            })
            batteryMessage = TextView(this@MainActivity).apply { textSize = 14f }
            addView(batteryMessage)
            batteryButton = Button(this@MainActivity).apply {
                text = BatteryOptimizationPolicy.ACTION_TEXT
                minHeight = dp(48)
                setOnClickListener { openBatteryOptimizationSettings() }
            }
            addView(batteryButton)
            // §26.1: theme choice (System default). Saved per user; applied by recreating the
            // current screen with the tab and input preserved (no network/VPN side effects).
            addView(TextView(this@MainActivity).apply {
                text = "Тема"
                textSize = 18f
                setPadding(0, dp(20), 0, 0)
            })
            val themeGroup = RadioGroup(this@MainActivity).apply { orientation = RadioGroup.VERTICAL }
            fun themeRadio(id: Int, label: String) = RadioButton(this@MainActivity).apply {
                this.id = id; text = label; minHeight = dp(48)
            }
            themeGroup.addView(themeRadio(THEME_SYSTEM, "Системная"))
            themeGroup.addView(themeRadio(THEME_LIGHT, "Светлая"))
            themeGroup.addView(themeRadio(THEME_DARK, "Тёмная"))
            when (AppTheme.load(this@MainActivity)) {
                ThemeMode.LIGHT -> themeGroup.check(THEME_LIGHT)
                ThemeMode.DARK -> themeGroup.check(THEME_DARK)
                ThemeMode.SYSTEM -> themeGroup.check(THEME_SYSTEM)
            }
            themeGroup.setOnCheckedChangeListener { _, checkedId ->
                val mode = when (checkedId) {
                    THEME_LIGHT -> ThemeMode.LIGHT
                    THEME_DARK -> ThemeMode.DARK
                    else -> ThemeMode.SYSTEM
                }
                if (AppTheme.load(this@MainActivity) != mode) {
                    AppTheme.save(this@MainActivity, mode)
                    recreate()
                }
            }
            addView(themeGroup)
            // §26.5: catalog auto-update schedule. Off is the default; changing the mode
            // replaces the single existing JobScheduler entry and reports a refusal honestly.
            addView(TextView(this@MainActivity).apply {
                text = "Обновление каталога"
                textSize = 18f
                setPadding(0, dp(20), 0, 0)
            })
            val scheduleGroup = RadioGroup(this@MainActivity).apply { orientation = RadioGroup.VERTICAL }
            fun scheduleRadio(id: Int, label: String) = RadioButton(this@MainActivity).apply {
                this.id = id; text = label; minHeight = dp(48)
            }
            scheduleGroup.addView(scheduleRadio(SCHEDULE_OFF, CatalogRefreshPolicy.label(CatalogRefreshMode.OFF)))
            scheduleGroup.addView(scheduleRadio(SCHEDULE_TWICE, CatalogRefreshPolicy.label(CatalogRefreshMode.TWICE_DAILY)))
            scheduleGroup.addView(scheduleRadio(SCHEDULE_DAILY, CatalogRefreshPolicy.label(CatalogRefreshMode.DAILY)))
            scheduleGroup.addView(scheduleRadio(SCHEDULE_WEEKLY, CatalogRefreshPolicy.label(CatalogRefreshMode.WEEKLY)))
            scheduleGroup.check(when (CatalogRefreshScheduleState.mode()) {
                CatalogRefreshMode.OFF -> SCHEDULE_OFF
                CatalogRefreshMode.TWICE_DAILY -> SCHEDULE_TWICE
                CatalogRefreshMode.DAILY -> SCHEDULE_DAILY
                CatalogRefreshMode.WEEKLY -> SCHEDULE_WEEKLY
            })
            scheduleStatus = TextView(this@MainActivity).apply {
                textSize = 14f
                setTextColor(TerlimoCatalogBrandTokens.MUTED_TEXT.toInt())
            }
            scheduleStatus.text = CatalogRefreshPolicy.statusText(CatalogRefreshScheduleState.mode(), catalogScheduleRefused)
            scheduleGroup.setOnCheckedChangeListener { _, checkedId ->
                val mode = when (checkedId) {
                    SCHEDULE_TWICE -> CatalogRefreshMode.TWICE_DAILY
                    SCHEDULE_DAILY -> CatalogRefreshMode.DAILY
                    SCHEDULE_WEEKLY -> CatalogRefreshMode.WEEKLY
                    else -> CatalogRefreshMode.OFF
                }
                if (CatalogRefreshScheduleState.mode() != mode) {
                    CatalogRefreshScheduleState.setMode(mode)
                    val result = runCatching { CatalogRefreshScheduler.reconcile(this@MainActivity, mode) }.getOrNull()
                    catalogScheduleRefused = if (result == null) {
                        mode != CatalogRefreshMode.OFF
                    } else {
                        CatalogRefreshPolicy.refused(mode, result.decision, result.scheduleResult)
                    }
                    scheduleStatus.text = CatalogRefreshPolicy.statusText(mode, catalogScheduleRefused)
                }
            }
            addView(scheduleGroup)
            addView(scheduleStatus)
            // §26.2: auto-connect to the last confirmed server on a user launch. Off by
            // default and independent from the §26.5 schedule: turning the schedule off
            // never cancels a pending explicit connect, and this switch never starts a job.
            autoConnectToggle = CheckBox(this@MainActivity).apply {
                text = "Подключаться автоматически к последнему серверу"
                isChecked = AutoConnectPrefs.isEnabled(this@MainActivity)
                minHeight = dp(48)
                setOnCheckedChangeListener { _, checked ->
                    AutoConnectPrefs.setEnabled(this@MainActivity, checked)
                    // §26.2 Off invalidates the live token and drops any pending sequence in
                    // the Service; it does not touch the independent §26.5 schedule.
                    if (!checked) cancelAutoConnectLaunch()
                }
            }
            addView(autoConnectToggle)
            addView(TextView(this@MainActivity).apply {
                text = "Подключение начнётся при следующем открытии приложения."
                textSize = 13f
                setTextColor(TerlimoCatalogBrandTokens.MUTED_TEXT.toInt())
            })
        }
        val settingsSurface = ScrollView(this).apply {
            isFillViewport = true
            setBackgroundColor(TerlimoCatalogBrandTokens.BACKGROUND.toInt())
            visibility = View.GONE
            settingsPanel.visibility = View.VISIBLE
            addView(settingsPanel)
        }
        val bottomNavigation = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            setBackgroundColor(TerlimoCatalogBrandTokens.BACKGROUND.toInt())
            setPadding(dp(8), dp(4), dp(8), dp(8))
            val destinations = mutableListOf<Button>()
            val destinationButtons = LinkedHashMap<NavTarget, Button>()
            fun destination(label: String, icon: Int, action: () -> Unit) = Button(this@MainActivity).apply {
                text = label
                configureBottomNavigationButton(icon)
                destinations += this
                setOnClickListener { action() }
            }
            val surfaces = listOf(mainSurface, subscriptionSurface, referralSurface, settingsSurface, helpSurface)
            fun showSurface(visible: View) = surfaces.forEach {
                it.visibility = if (it === visible) View.VISIBLE else View.GONE
            }
            var navState = BottomNavigation.initial()
            fun onTab(target: NavTarget) {
                navState = BottomNavigation.onTap(navState, target)
                visibleTarget = navState.visible
                val visibleView = when (navState.visible) {
                    NavTarget.HOME -> mainSurface
                    NavTarget.SUBSCRIPTION -> subscriptionSurface
                    NavTarget.SETTINGS -> settingsSurface
                    NavTarget.HELP -> helpSurface
                    NavTarget.REFERRAL -> referralSurface
                }
                showSurface(visibleView)
                destinationButtons.forEach { (tab, button) ->
                    button.setBottomNavigationActive(tab == navState.selected)
                }
                if (NavigationServiceCommands.shouldCancelCommonPing(
                        navState.visible != NavTarget.HOME, SessionService.hasActiveCommonPing())) {
                    // Leaving the catalog screen stops an in-flight common ping; navigation with
                    // no running ping never wakes the service.
                    startService(Intent(this@MainActivity, SessionService::class.java)
                        .setAction(NavigationServiceCommands.CANCEL_COMMON_PING))
                }
                if (NavigationServiceCommands.shouldRequestAnnouncements(
                        target == NavTarget.HELP, SessionService.hasLiveAttempt())) {
                    // §11: opening the section asks the existing attempt for a fresh list;
                    // without a live attempt there is nothing to ask and nothing to wake.
                    startService(Intent(this@MainActivity, SessionService::class.java)
                        .setAction(NavigationServiceCommands.REQUEST_ANNOUNCEMENTS))
                }
            }
            openTab = { target -> onTab(target) }
            openHelp = { onTab(NavTarget.HELP) }
            BottomNavigation.destinations.forEach { navDestination ->
                val button = destination(navDestination.label, navDestination.iconRes) { onTab(navDestination.target) }
                destinationButtons[navDestination.target] = button
                addView(button, LinearLayout.LayoutParams(0, dp(64), 1f))
            }
            // Bind only after the map is populated, so the first render(SessionService.view)
            // already holds a non-null Help button and can apply the unread red dot.
            helpTabButton = destinationButtons[NavTarget.HELP]
            // §26.1: restore AFTER the buttons exist, so the selected highlight is rendered
            // too. The pure selection helper keeps Routing out of the in-place surfaces and
            // never runs a side-effecting onTab (no probe cancel / routing launch / ask).
            val restoredState = BottomNavigation.restoreSelection(navState, restoreTarget)
            if (restoredState !== navState) {
                navState = restoredState
                val visibleView = when (restoredState.visible) {
                    NavTarget.REFERRAL -> referralSurface
                    NavTarget.SUBSCRIPTION -> subscriptionSurface
                    NavTarget.SETTINGS -> settingsSurface
                    NavTarget.HELP -> helpSurface
                    else -> mainSurface
                }
                surfaces.forEach { it.visibility = if (it === visibleView) View.VISIBLE else View.GONE }
                destinationButtons.forEach { (tab, button) ->
                    button.setBottomNavigationActive(tab == restoredState.selected)
                }
                visibleTarget = navState.visible
            } else if (restoreTarget == null) {
                // A normal cold start (no recreate) opens Главная.
                onTab(NavTarget.HOME)
            }
            restoreTarget = null
        }
        val content = FrameLayout(this).apply {
            addView(mainSurface, FrameLayout.LayoutParams(FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT))
            addView(referralSurface, FrameLayout.LayoutParams(FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT))
            addView(subscriptionSurface, FrameLayout.LayoutParams(FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT))
            addView(helpSurface, FrameLayout.LayoutParams(FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT))
            addView(settingsSurface, FrameLayout.LayoutParams(FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT))
        }
        // §26.2: the launch token is evaluated only after the screen exists, and only for a
        // genuine user open (ACTION_MAIN/LAUNCHER), never for an external deep link or a return.
        if (autoConnectUserLaunch) maybeAutoConnect()
        setContentView(LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setBackgroundColor(TerlimoCatalogBrandTokens.BACKGROUND.toInt())
            addView(content, LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, 0, 1f))
            addView(bottomNavigation, LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT))
        })
        render(SessionService.view)
        maybeOpenAnnouncements(intent)
        autoLoadSavedSubscription()
    }

    /**
     * Owner contract 4: the first render of the process starts the standard linkless
     * bootstrap/resume through the existing seed gate, so /me and the gateways load
     * automatically on first open. Exactly once per process; the
     * cached list never cancels the refresh (existing `resume` action unchanged).
     */
    private fun autoLoadSavedSubscription() {
        if (!AutoLoadPolicy.shouldStart(projectedState().nodes)) return
        if (!ProcessAutoLoad.claim()) return
        startForegroundService(Intent(this, SessionService::class.java).setAction("resume"))
    }

    /**
     * §26.3: the Settings entry opens the standard Android VPN screen, where the user alone
     * decides whether to enable Always-on. If the OEM resolves no VPN settings screen, the
     * generic system settings are shown instead; if neither resolves, a clear message is
     * shown. Nothing is enabled from here and no reboot/autostart behaviour is added.
     */
    /**
     * §26.4: platform check of THIS app's battery-optimization state. A missing manager or a
     * thrown SecurityException/RuntimeException is an honest UNKNOWN, never a false PASS.
     */
    private fun batteryIgnoringOptimization(): Boolean? = try {
        (getSystemService(POWER_SERVICE) as? PowerManager)?.isIgnoringBatteryOptimizations(packageName)
    } catch (_: SecurityException) {
        null
    } catch (_: RuntimeException) {
        null
    }

    private fun renderBatteryOptimization() {
        val state = BatteryOptimizationPolicy.state(batteryIgnoringOptimization())
        val visible = BatteryOptimizationPolicy.warningVisible(state)
        batteryMessage.text = BatteryOptimizationPolicy.text(state)
        batteryMessage.visibility = if (visible) View.VISIBLE else View.GONE
        batteryButton.visibility = if (visible) View.VISIBLE else View.GONE
    }

    /**
     * §26.4: opens the standard battery screen where the user alone decides. Falls back to the
     * app details and generic settings; if none resolves a clear message is shown. No new
     * permission (the ignore-optimization request screen needs none) and no app-side change.
     */
    private fun openBatteryOptimizationSettings() {
        for (action in BatteryOptimizationPolicy.actions()) {
            val intent = Intent(action).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            // Only the app-details screen takes the package; the battery list and generic
            // settings screens are package-agnostic.
            if (action == Settings.ACTION_APPLICATION_DETAILS_SETTINGS) {
                intent.data = android.net.Uri.parse("package:$packageName")
            }
            try {
                startActivity(intent)
                return
            } catch (_: ActivityNotFoundException) {
            } catch (_: SecurityException) {
            }
        }
        Toast.makeText(this, "Системные настройки батареи недоступны на этом устройстве",
            Toast.LENGTH_LONG).show()
    }

    private fun helpLink(label: String, url: String) = Button(this).apply {
        text = label
        minHeight = dp(48)
        setOnClickListener { openHelpLink(url) }
    }

    /** §27: open a confirmed public contact/legal page in the existing external app. */
    private fun openHelpLink(url: String) {
        val parsed = android.net.Uri.parse(url)
        if (!url.startsWith("https://") || parsed.host.isNullOrBlank()) return
        try {
            startActivity(Intent(Intent.ACTION_VIEW, parsed))
        } catch (_: android.content.ActivityNotFoundException) {
            Toast.makeText(this, "Не удалось открыть ссылку", Toast.LENGTH_LONG).show()
        }
    }

    private fun Button.configureBottomNavigationButton(icon: Int) {
        minWidth = 0
        minHeight = dp(48)
        isAllCaps = false
        textSize = 11f
        // 720px @320dpi gives ~68.8dp per equal-weight slot; allow the long label
        // «Маршрутизация» to wrap to a second line instead of being clipped. Approved
        // texts are unchanged; only local wrap/height behaviour is adjusted.
        maxLines = 2
        ellipsize = null
        gravity = android.view.Gravity.CENTER
        background = null
        stateListAnimator = null
        elevation = 0f
        setCompoundDrawablesRelativeWithIntrinsicBounds(0, icon, 0, 0)
        compoundDrawablePadding = dp(2)
        setPadding(dp(2), dp(4), dp(2), dp(2))
        // §26.1: palette-driven default tint/text so icons are never hard-coded white.
        setBottomNavigationActive(false)
    }
    private fun Button.setBottomNavigationActive(active: Boolean) {
        val color = if (active) TerlimoCatalogBrandTokens.ACCENT.toInt() else TerlimoCatalogBrandTokens.MUTED_TEXT.toInt()
        setTextColor(color)
        compoundDrawableTintList = ColorStateList.valueOf(color)
        isSelected = active
    }
    private fun dp(value: Int) = (value * resources.displayMetrics.density + .5f).toInt()
    private fun requestNodeSelection(chosen: String) {
        val state = SessionService.view
        // Owner contract 5: a browse row is a host-local preference for the next explicit
        // connect. Tap only remembers it; no probe, no select_node and no attempt starts.
        if (BrowseCatalogCodec.displayedNodes(state).any { it.id == chosen }) {
            startService(Intent(this, SessionService::class.java)
                .setAction("browse_select").putExtra("node_id", chosen))
            return
        }
        // UX §5: with the protection held (KillSwitch) another gateway must stay selectable
        // without a Disconnect; the explicit tap starts a fresh attempt for the chosen node.
        val switchPhase = state.phase == "Connected"
        if (state.phase !in setOf("CatalogReady", "Connected", "KillSwitch") ||
            state.nodes.none { it.id == chosen }) {
            if (switchPhase) {
                SwitchDiagnostics.log("ui_reject",
                    if (state.nodes.none { it.id == chosen }) "unknown_target" else "phase")
            }
            return
        }
        val displayed = pendingChoiceId ?: state.pendingNodeId ?: NodeSelection.displayedNodeId(state.nodes, state.selectedNodeId)
        if (chosen == displayed) {
            if (switchPhase) {
                SwitchDiagnostics.log("ui_reject", when {
                    pendingChoiceId != null -> "local_pending"
                    state.pendingNodeId != null -> "service_pending"
                    else -> "same_target"
                })
            }
            return
        }
        pendingChoiceId = chosen
        selectedForConsent = null
        render(state)
        if (switchPhase) SwitchDiagnostics.log("ui_request")
        startService(Intent(this@MainActivity, SessionService::class.java)
            .setAction(when (state.phase) {
                "Connected" -> "switch"
                "KillSwitch" -> "hold_select"
                else -> "choose"
            })
            .putExtra("node_id", chosen))
    }
    /** §11: a system-notification tap opens the Help «Уведомления» section. */
    private fun maybeOpenAnnouncements(incoming: Intent?) {
        val extra = incoming?.getBooleanExtra(AnnouncementDeepLink.EXTRA_OPEN_ANNOUNCEMENTS, false) == true
        if (AnnouncementDeepLink.shouldOpen(extra)) openHelp?.invoke()
    }

    /**
     * §11 display-only Help section. It reads only the accepted server snapshot: expired
     * messages are hidden, unread after local acks drives the red dot, and an unreadable
     * announcement id is shown without a false "read" affordance.
     */
    /**
     * §§18–19 connected devices: server-owned list, one explicit delete of a chosen non-current
     * device, the freed-slot bind action reusing the existing registration link, and the honest
     * "subscription unavailable for this device" state after the current device is removed.
     */
    private fun renderDevices(state: ViewState) {
        val devices = state.devices
        val canManage = DevicesPolicy.canManage(state.accountAccess?.projection?.registration)
        // Before the first confirmed projection in this service lifetime the proof is unknown,
        // not absent: the explicit refresh may start the bounded cold attempt and the fresh
        // /me decides (a non-registered answer is surfaced honestly, never guessed).
        devicesRefreshButton.visibility =
            if (canManage || state.accountAccess == null) View.VISIBLE else View.GONE
        devicesCount.text = DevicesPolicy.countLine(devices).orEmpty()
        devicesCount.visibility = if (devicesCount.text.isNullOrEmpty()) View.GONE else View.VISIBLE
        devicesBlock.removeAllViews()
        if (DevicesPolicy.currentUnavailable(devices)) {
            devicesBlock.addView(TextView(this).apply { text = "Для этого устройства подписка недоступна." })
        }
        DevicesPolicy.errorText(devices?.error)?.let {
            devicesBlock.addView(TextView(this).apply { text = it })
        }
        DevicesPolicy.deleteStateText(devices?.lastDelete)?.let {
            devicesBlock.addView(TextView(this).apply { text = it })
        }
        val deleteInFlight = devices?.deleteInFlight == true
        devices?.devices.orEmpty().forEach { row ->
            val parts = listOfNotNull(
                row.name ?: "Устройство",
                row.platform,
                if (row.isCurrent) "Это устройство" else null,
                if (row.status == "revoked") "отозвано" else null,
            )
            val rowView = LinearLayout(this).apply {
                orientation = LinearLayout.HORIZONTAL
                setPadding(0, dp(4), 0, dp(4))
            }
            rowView.addView(TextView(this).apply {
                text = parts.joinToString(" · ")
                layoutParams = LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f)
            })
            if (!row.isCurrent && row.status == "active") {
                val pending = deleteInFlight && devices?.pendingDeviceId == row.deviceId
                rowView.addView(Button(this).apply {
                    text = if (pending) "Удаляем…" else "Удалить"
                    minHeight = dp(48)
                    isEnabled = !deleteInFlight
                    setOnClickListener {
                        startForegroundService(Intent(this@MainActivity, SessionService::class.java)
                            .setAction("device_delete").putExtra("device_id", row.deviceId))
                    }
                })
            }
            devicesBlock.addView(rowView)
        }
        if (canManage && DevicesPolicy.canBind(devices)) {
            devicesBlock.addView(Button(this).apply {
                text = "Привязать это устройство"; minHeight = dp(48)
                setOnClickListener {
                    startForegroundService(Intent(this@MainActivity, SessionService::class.java)
                        .setAction("telegram_register"))
                }
            })
        }
    }

    private fun renderAnnouncements(state: ViewState) {
        val granted = checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED
        val now = System.currentTimeMillis()
        val unread = AnnouncementsPolicy.unreadCount(state.announcements, now)
        announcementsBadge.text = if (unread > 0) "● $unread" else ""
        val visible = AnnouncementsCodec.visible(state.announcements.snapshot?.announcements.orEmpty(), now)
        // The denied-permission note is tied to a visible current message, never shown empty.
        val note = AnnouncementsText.deniedPermissionNote(granted, visible.isNotEmpty())
        announcementsDenied.text = note.orEmpty()
        announcementsDenied.visibility = if (note == null) View.GONE else View.VISIBLE
        announcementsEmpty.visibility = if (visible.isEmpty()) View.VISIBLE else View.GONE
        // §28: the unread red dot also lives on the persistent Help tab button.
        helpTabButton?.text = BottomNavigation.unreadLabel("Помощь", NavTarget.HELP, unread)
        announcementsList.removeAllViews()
        homeAnnouncements.removeAllViews()
        for (announcement in visible) {
            announcementsList.addView(announcementRow(announcement, state))
            if (AnnouncementActions.task(announcement.action.type) == AnnouncementTask.REFRESH_CATALOG) {
                homeAnnouncements.addView(announcementRow(announcement, state))
            }
        }
    }

    private fun announcementRow(announcement: Announcement, state: ViewState): View {

        val read = announcement.id in state.announcements.readIds || !announcement.unread
        val row = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(0, dp(12), 0, dp(12))
        }
        row.addView(TextView(this).apply { text = announcement.title; textSize = 16f })
        row.addView(TextView(this).apply { text = announcement.text })
        row.addView(TextView(this).apply { text = AnnouncementsText.dateText(announcement.validUntil); textSize = 12f })
        if (!read) {
            if (AnnouncementsCodec.unreservedPathId(announcement.id)) {
                row.addView(Button(this).apply {
                    text = "Отметить прочитанным"
                    minHeight = dp(48)
                    setOnClickListener {
                        startService(Intent(this@MainActivity, SessionService::class.java)
                            .setAction("announcement_read")
                            .putExtra("announcement_id", announcement.id))
                    }
                })
            } else {
                row.addView(TextView(this).apply { text = AnnouncementsText.unreadableNote() })
            }
        }
        val label = AnnouncementsText.actionLabel(announcement.action)
        val task = AnnouncementActions.task(announcement.action.type)
        if (label != null && task != AnnouncementTask.NONE) {
            row.addView(Button(this).apply {
                text = label
                minHeight = dp(48)
                isEnabled = AnnouncementActions.enabled(task, paymentsSurface = true, supportSurface = false)
                setOnClickListener {
                    when (task) {
                        AnnouncementTask.REFRESH_CATALOG -> startService(Intent(this@MainActivity, SessionService::class.java).setAction("refresh"))
                        AnnouncementTask.OPEN_PAYMENTS -> openTab?.invoke(NavTarget.SUBSCRIPTION)
                        else -> Unit
                    }
                }
            })
        }
        return row
    }

    /** Live view, or the durable retained catalog projected for a fresh process. */
    private fun projectedState(): ViewState =
        RetainedProjection.hydrate(SessionService.view,
            runCatching { installationStore.readCatalogCache() }.getOrNull())

    /**
     * §26.2 one launch attempt: guarded by the launch token, the master pref and the live
     * phase (never while connected/connecting). Consent is requested here (Activity-only);
     * the native command is sent by the Service, which re-validates the stored last server
     * against the current account and shows an honest message when it is unusable.
     */
    /** A genuine user open (cold ACTION_MAIN or a launcher re-open), never an external deep link. */
    private fun isUserLaunchIntent(incoming: Intent?): Boolean {
        val action = incoming?.action
        if (action != null && action != Intent.ACTION_MAIN) return false
        return true
    }

    /**
     * §26.2 one launch attempt. Arms a fresh token, requests the Activity-only consent if
     * needed, then hands the token to the Service. The Service re-validates the preference,
     * the token generation and the account scope before any choose/select.
     */
    private fun maybeAutoConnect() {
        if (!AutoConnectPrefs.isEnabled(this) || autoConnectLaunch.started()) return
        val generation = AutoConnectPrefs.armLaunch(this)
        if (autoConnectLaunch.start(generation) == 0L) return
        val consent = VpnService.prepare(this)
        if (consent != null) {
            autoConnectLaunch.needsConsent()
            startActivityForResult(consent, REQUEST_AUTOCONNECT_CONSENT)
        } else {
            sendAutoConnect(generation)
            autoConnectLaunch.sent()
        }
    }

    private fun sendAutoConnect(generation: Long) {
        startForegroundService(Intent(this, SessionService::class.java)
            .setAction("autoconnect").putExtra(AutoConnectPrefs.EXTRA_GENERATION, generation))
    }

    /** Off / Disconnect / cancel: invalidate the token and drop any pending sequence.
     * A cold Off toggle must not create the service or promote a foreground notification;
     * the preference/token already block every queued send. */
    private fun cancelAutoConnectLaunch() {
        autoConnectLaunch.invalidate()
        AutoConnectPrefs.invalidate(this)
        if (SessionService.isRunning()) {
            startService(Intent(this, SessionService::class.java).setAction("autoconnect_cancel"))
        }
    }

    private fun requestVpnConsentForCurrentSelection() {
        val state = projectedState()
        val nodeId = NodeSelection.connectableNodeId(
            BrowseCatalogCodec.verifiedNodes(state), state.selectedNodeId)
        PowerDiagnostics.line("rvcs.enter",
            "nodeFound" to (nodeId != null).toString(),
            "phase" to state.phase,
            "live" to SessionService.view.phase,
            "display" to state.displayMode,
            "nodes" to state.nodes.size.toString())
        if (nodeId == null) {
            // Waiting for explicit first connect: no node selection exists yet. The
            // eligible /me (data_access=none, onboarding not_started) may still start
            // the first-connect consent; the separate explicit action follows consent.
            PowerDiagnostics.line("rvcs.noNode", "phase" to state.phase)
            requestPreAdmissionConsent(state)
            return
        }
        val ready = state.phase == "CatalogReady"
        val retained = RetainedCatalogPolicy.connectableId(state) == nodeId
        PowerDiagnostics.flag("rvcs.guard",
            "ready" to ready, "retained" to retained,
            "pendChoice" to (pendingChoiceId != null),
            "pendNode" to (state.pendingNodeId != null))
        if ((!ready && !retained) || pendingChoiceId != null || state.pendingNodeId != null) {
            PowerDiagnostics.line("rvcs.refused", "phase" to state.phase)
            return
        }
        consentDenied = false
        selectedForConsent = nodeId
        val consent = VpnService.prepare(this)
        PowerDiagnostics.flag("rvcs.consent", "needsConsent" to (consent != null))
        if (consent != null) startActivityForResult(consent, 100) else connectSelected()
    }

    /**
     * Consent gate of the pre-admission first connect. It only starts the mandatory
     * VPN consent for the eligible waiting state; the explicit native command is sent
     * only from [connectSelected] after a granted consent.
     */
    private fun requestPreAdmissionConsent(state: ViewState) {
        PowerDiagnostics.flag("rpac.enter", "connectable" to PreAdmissionConnect.connectable(state, pendingChoiceId != null))
        if (!PreAdmissionConnect.connectable(state, pendingChoiceId != null)) {
            PowerDiagnostics.line("rpac.refused", "phase" to state.phase)
            return
        }
        // Owner contract 5: with a non-empty browse list the connect waits for an explicit
        // row selection; the chosen gateway_id travels as the optional gateway_key.
        if (BrowseConnectGate.requiresSelection(state)) {
            PowerDiagnostics.line("rpac.requiresSelection", "phase" to state.phase)
            status.text = "Выберите сервер из списка и нажмите подключение"
            return
        }
        consentDenied = false
        preAdmissionConsentRequest = true
        // Explicit owner warning before the first hour starts; display-only text.
        if (PreAdmissionConnect.eligible(state)) status.text = PreAdmissionConnect.HOUR_WARNING
        val consent = VpnService.prepare(this)
        PowerDiagnostics.flag("rpac.consent", "needsConsent" to (consent != null))
        if (consent != null) startActivityForResult(consent, 100) else connectSelected()
    }
    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        val launcherOpen = intent.action == Intent.ACTION_MAIN &&
            (intent.categories?.contains(Intent.CATEGORY_LAUNCHER) == true)
        setIntent(intent)
        maybeOpenAnnouncements(intent)
        if (launcherOpen) {
            autoConnectLaunch.invalidate()
            maybeAutoConnect()
        }
    }
    override fun onSaveInstanceState(outState: Bundle) {
        outState.putString(STATE_VISIBLE_TAB, visibleTarget.name)
        outState.putLong(STATE_AUTOCONNECT_GENERATION, autoConnectLaunch.currentGeneration())
        outState.putBoolean(STATE_AUTOCONNECT_CONSENT_PENDING, autoConnectLaunch.started())
        checkoutOpenPolicy.saveTo(outState)
        super.onSaveInstanceState(outState)
    }
    override fun onStart() {
        super.onStart()
        appUpdateUi.start()
        statusTickStarted = true
        SessionService.listeners.add(listener)
        // render() is the single arm point: an already accepted hour arms now, and a later
        // accepted/render transition arms through the listener without any native event.
        render(SessionService.view)
    }
    override fun onStop() {
        statusTickStarted = false
        main.removeCallbacks(statusTick)
        SessionService.listeners.remove(listener)
        appUpdateUi.stop()
        super.onStop()
    }

    /**
     * Returning from the external Telegram registration triggers exactly one server status
     * refresh while a registration is pending. No other lifecycle action is added.
     */
    override fun onResume() {
        super.onResume()
        AppForeground.isForeground = true
        appUpdateUi.resume()
        // Official v20 return path: exactly one pending manual CAPTCHA window is shown again.
        ManlCaptchaWebViewManager.checkAndShowPendingCaptcha(this)
        // §26.4: refresh the battery warning on every return from the system settings so a
        // lifted restriction disappears immediately.
        if (::batteryMessage.isInitialized) renderBatteryOptimization()
        if (SessionService.view.registration?.state == "pending") {
            startForegroundService(Intent(this, SessionService::class.java).setAction("telegram_refresh"))
        }
    }

    override fun onPause() {
        AppForeground.isForeground = false
        super.onPause()
    }
    @Deprecated("Activity result used without adding an activity framework")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == REQUEST_AUTOCONNECT_CONSENT) {
            val generation = autoConnectLaunch.currentGeneration()
            val stillValid = resultCode == RESULT_OK && generation != 0L &&
                AutoConnectPrefs.isEnabled(this) && generation == AutoConnectPrefs.generation(this)
            if (autoConnectLaunch.consentReturned(generation, stillValid)) sendAutoConnect(generation)
            else status.text = UserStatusText.error(AutoConnectCode.CONSENT)
            return
        }
        if (requestCode != 100) return
        PowerDiagnostics.flag("consent.result", "ok" to (resultCode == RESULT_OK))
        if (resultCode == RESULT_OK) connectSelected()
        else {
            selectedForConsent = null
            preAdmissionConsentRequest = false
            consentDenied = true
            render(SessionService.view)
        }
    }
    private fun connectSelected() {
        PowerDiagnostics.flag("cs.enter",
            "preAdmission" to preAdmissionConsentRequest,
            "idPresent" to (selectedForConsent != null))
        if (preAdmissionConsentRequest) {
            // The separate explicit action after consent: no node id, no catalogue.
            // SessionService passes exactly this flag through the bridge to the runner.
            // The optional browsed gateway_key selects the explicit connect target.
            preAdmissionConsentRequest = false
            // Owner contract 5: re-check the current browse selection after the consent
            // round-trip; without a current selection the keyless fallback is refused.
            if (BrowseConnectGate.requiresSelection(SessionService.view)) {
                PowerDiagnostics.line("cs.pre.requiresSelection", "phase" to SessionService.view.phase)
                status.text = "Выберите сервер из списка и повторите подключение"
                render(SessionService.view)
                return
            }
            if (!PreAdmissionConnect.connectable(SessionService.view, pendingChoiceId != null)) return
            val gatewayKey = BrowseCatalogCodec.selectedId(SessionService.view)
            PowerDiagnostics.flag("cs.pre.send", "keyPresent" to gatewayKey.isNotEmpty())
            val intent = Intent(this, SessionService::class.java).setAction("onboarding_connect")
            if (gatewayKey.isNotEmpty()) intent.putExtra("gateway_key", gatewayKey)
            startForegroundService(intent)
            return
        }
        val id = selectedForConsent
        if (id == null) {
            PowerDiagnostics.line("cs.noId", "phase" to SessionService.view.phase)
            return
        }
        selectedForConsent = null
        val state = projectedState()
        if (id != NodeSelection.connectableNodeId(state.nodes, state.selectedNodeId)) {
            PowerDiagnostics.line("cs.idMismatch", "phase" to state.phase)
            return
        }
        when {
            state.phase == "CatalogReady" && pendingChoiceId == null && state.pendingNodeId == null -> {
                PowerDiagnostics.line("cs.branch", "value" to "select", "phase" to state.phase)
                startService(Intent(this, SessionService::class.java).setAction("select").putExtra("node_id", id))
            }
            RetainedCatalogPolicy.connectableId(state.phase, state.nodes, state.selectedNodeId) == id -> {
                // Cold start: the retained id starts a fresh native attempt (foreground
                // service is required by the OS for a cold started service).
                PowerDiagnostics.line("cs.branch", "value" to "connect", "phase" to state.phase)
                startForegroundService(Intent(this, SessionService::class.java).setAction("connect").putExtra("node_id", id))
            }
            else -> PowerDiagnostics.line("cs.branch", "value" to "noBranch", "phase" to state.phase)
        }
    }
    private fun referralAction(action: String) {
        startForegroundService(Intent(this, SessionService::class.java).setAction(action))
    }

    private fun openRecoveryEditor() {
        val seed = runCatching { MobileBootstrapSeed.parse(assets.open("test-mobile.json").bufferedReader().use { it.readText() }) }.getOrNull()
        val current = SessionService.view
        val reason = RecoveryCodeUi.unavailableReason(seed?.recoveryVerifyKeyB64 != null && seed.serviceSeed != null &&
            installationStore.read().optString("link").isEmpty(),
            workingVpn = current.phase in setOf("Connected", "SwitchingServer", "KillSwitch", "SleepPaused", "Reconnecting"),
            busy = current.recoveryStatus == "RECOVERY_RUNNING",
            serviceActive = current.attempt != null || current.phase !in setOf("Idle", "Error"))
        if (reason != null) {
            Toast.makeText(this, RecoveryCodeUi.textForStatus(reason), Toast.LENGTH_LONG).show()
            return
        }
        RecoveryCodeUi.showEditor(this, submit = { code ->
            cancelAutoConnectLaunch()
            startForegroundService(Intent(this, SessionService::class.java)
                .setAction("recovery_apply").putExtra("recovery_code", code))
        }, openTelegram = { FirstReleaseLinks.recoveryBotUrl()?.let(::openHelpLink) },
            openWebsite = { FirstReleaseLinks.recoverySiteUrl()?.let(::openHelpLink) })
    }

    private fun render(passed: ViewState) {
        // Live view, or the durable retained catalog when the Service is not running
        // (fresh process). Display-only; the Service is started by the Connect action.
        val state = projectedState()
        // A traffic-only change updates just the header metrics: never a full screen rebuild.
        val previous = lastRenderedState
        if (previous != null && previous.copy(traffic = state.traffic) == state) {
            orbitHeader.setTraffic(state.traffic)
            lastRenderedState = state
            return
        }
        lastRenderedState = state
        val selectable = state.phase in setOf("CatalogReady", "Connected", "SwitchingServer", "KillSwitch")
        if (!selectable) consentDenied = false
        if (!selectable) {
            pendingChoiceId = null
        } else if (pendingChoiceId == null && state.pendingNodeId != null) {
            pendingChoiceId = state.pendingNodeId
        } else if (pendingChoiceId != null && state.pendingNodeId == null &&
            (pendingChoiceId == state.selectedNodeId || state.nodes.none { it.id == pendingChoiceId } || state.error != null)) {
            pendingChoiceId = null
        }
        val pending = pendingChoiceId ?: state.pendingNodeId
        recoveryErrorButton.visibility = if (state.error != null) View.VISIBLE else View.GONE
        status.text = mainStatusText(state)
        subscription.text = SubscriptionStatusText.status(
            state.summary,
            state.accountAccess,
            android.os.SystemClock.elapsedRealtime(),
        )
        subscriptionTerm.text = state.accountAccess?.projection?.let {
            SubscriptionTermText.term(it, state.purchase, java.time.ZoneId.systemDefault())
        }.orEmpty()
        subscriptionTerm.visibility = if (subscriptionTerm.text.isNullOrEmpty()) View.GONE else View.VISIBLE
        subscriptionTraffic.text = ServerUsageText.accountTraffic(
            state.serverUsage, android.os.SystemClock.elapsedRealtime(), state.serverUsageUnavailable)
        subscriptionTraffic.visibility =
            if (subscriptionTraffic.text.isNullOrEmpty()) View.GONE else View.VISIBLE
        subscriptionDevices.text = DevicesPolicy.reconciledDeviceLine(
            state.devices,
            state.attempt,
            state.accountAccess?.projection?.account?.accountRef,
            state.accountAccess?.current == true &&
                state.accountAccess?.projection?.entitlement?.status == "active",
            SubscriptionDeviceText.line(state.accountAccess),
        ).orEmpty()
        subscriptionDevices.visibility = if (subscriptionDevices.text.isNullOrEmpty()) View.GONE else View.VISIBLE
        subscriptionPlan.text = SubscriptionPlanText.line(state.accountAccess?.projection).orEmpty()
        subscriptionPlan.visibility = if (subscriptionPlan.text.isNullOrEmpty()) View.GONE else View.VISIBLE
        renderDevices(state)
        renderPurchase(state)
        val displayed = pending ?: NodeSelection.displayedNodeId(state.nodes, state.selectedNodeId)
        val verifiedConnectable = NodeSelection.connectableNodeId(
            BrowseCatalogCodec.verifiedNodes(state), state.selectedNodeId)
        connectButton.isEnabled = (state.phase == "CatalogReady" && pending == null && verifiedConnectable != null) ||
            (pending == null && RetainedCatalogPolicy.connectableId(state) != null) ||
            BrowseConnectGate.connectable(state, pending != null)
        nodes.isEnabled = state.phase in setOf("CatalogReady", "Connected", "KillSwitch") && state.nodes.isNotEmpty()
        captchaButton.visibility = if (SessionService.captchaPending) View.VISIBLE else View.GONE
        if (labels != state.nodes) {
            labels = state.nodes
            nodes.adapter = ArrayAdapter(this, android.R.layout.simple_spinner_dropdown_item,
                listOf(NodeSelection.PLACEHOLDER) + labels.map { it.name })
        }
        val target = if (displayed == null) 0 else NodeSelection.spinnerPosition(labels, displayed)
        if (nodes.selectedItemPosition != target) nodes.setSelection(target, false)
        catalogView.render(state, state.pings)
        renderAnnouncements(state)
        orbitHeader.render(state, pending != null)
        activateHourButton.visibility = if (PreAdmissionConnect.eligible(state) && PreAdmissionConnect.connectable(state, pending != null))
            View.VISIBLE else View.GONE
        registerTelegramButton.visibility = if (RegistrationUi.registerVisible(state)) View.VISIBLE else View.GONE
        registerTelegramButton.text = RegistrationUi.buttonText(state)
        loginTelegramButton.visibility = if (RegistrationUi.loginVisible(state)) View.VISIBLE else View.GONE
        trialButton.visibility = if (TrialUi.activateVisible(state)) View.VISIBLE else View.GONE
        trialButton.text = TrialUi.buttonText(state)
        val homeAction = HomeAccessAction.forState(state, pending != null)
        homeContextButton.text = homeAction.label
        homeContextButton.visibility = if (homeAction == HomeAccessAction.NONE) View.GONE else View.VISIBLE
        referralUi.render(ReferralClientPresentation.model(state))
        accountStatus.text = listOfNotNull(RegistrationUi.statusText(state), TrialUi.statusText(state)).joinToString("\n")
        armStatusTick()
    }

    /**
     * At most one queued display tick: every (re)arm removes the previous callback and posts
     * only when the accepted snapshot still counts down on a started Activity. A queued/late
     * render callback after onStop cannot arm because [statusTickStarted] is already false.
     */
    private fun armStatusTick() {
        main.removeCallbacks(statusTick)
        if (AccountAccessDisplayRefresh.shouldPost(statusTickStarted, SessionService.view.accountAccess,
                android.os.SystemClock.elapsedRealtime()))
            main.postDelayed(statusTick, AccountAccessDisplayRefresh.TICK_MILLIS)
    }

    /**
     * Explicit «Оплатить» tap: the only live marker that may auto-open the created payment.
     * It is armed with the exact quote id sent in this tap, so only the correlated
     * payment_create_result of this attempt can consume it; any payment kept in ViewState
     * (an older order) matches nothing.
     */
    private fun noteExplicitPay(sentQuoteId: String) {
        checkoutOpenPolicy.onPayRequested(sentQuoteId)
    }

    /** Explicit «Продолжить оплату» for the existing payment: same saved order, no new invoice. */
    private fun continueExistingPayment() {
        val payment = SessionService.view.purchase?.payment
        val open = checkoutOpenPolicy.onContinueRequested(payment)
        if (open != null) launchCheckoutBrowser(open)
        render(SessionService.view)
    }

    /**
     * §3.2B: open the provider-issued HTTPS checkout URL for an already-created payment.
     * The payment is recorded as opened only after `startActivity` actually succeeds, so a
     * failed launch stays retryable through the explicit continue action; no browser / no
     * network failure becomes an honest visible error and the kept payment stays
     * checkable. Returning from the browser never writes access.
     */
    private fun launchCheckoutBrowser(open: CheckoutOpen) {
        val parsed = android.net.Uri.parse(open.url)
        if (!open.url.startsWith("https://") || parsed.host.isNullOrBlank()) {
            checkoutOpenPolicy.onOpenFailed(PaymentsText.CHECKOUT_INVALID_LINK_TEXT)
            return
        }
        try {
            startActivity(Intent(Intent.ACTION_VIEW, parsed))
            checkoutOpenPolicy.onOpened(open.paymentId)
        } catch (_: android.content.ActivityNotFoundException) {
            checkoutOpenPolicy.onOpenFailed(PaymentsText.CHECKOUT_NO_BROWSER_TEXT)
        } catch (_: Exception) {
            checkoutOpenPolicy.onOpenFailed(PaymentsText.CHECKOUT_INVALID_LINK_TEXT)
        }
    }

    /** Display-only text refresh of the existing surfaces; it republishes no view state. */
    private fun refreshStatusTexts() {
        val state = SessionService.view
        status.text = mainStatusText(state)
        subscription.text = SubscriptionStatusText.status(
            state.summary, state.accountAccess, android.os.SystemClock.elapsedRealtime())
    }

    private fun purchaseBusy(): Boolean {
        return SessionService.view.purchase?.sending == true
    }

    private fun selectedPurchaseQuote(current: PurchaseState?): PaymentQuote? {
        val plan = current?.plans?.firstOrNull { it.planId == purchasePlanId }
        if (current?.selectedRenewExtraSlotIds != purchaseRenewExtraSlotIds.sorted()) return null
        return PurchaseFlow.payableQuote(current, plan, purchaseMethod, java.time.Instant.now())
            ?.takeIf { it.quoteId != purchaseRejectedQuoteId }
    }

    /** Back/Cancel abandon only the local selection, never the existing server order. */
    private var purchaseQuoteAfterConfirmation = false

    private fun clearPurchaseSelection(clearPlan: Boolean) {
        if (clearPlan) {
            purchasePlanId = null
            purchaseRenewExtraSlotIds = emptyList()
        }
        purchaseMethod = null
        displayedPurchaseQuote = null
        purchaseRejectedQuoteId = SessionService.view.purchase?.quote?.quoteId
        checkoutOpenPolicy.clearAwaiting()
        render(SessionService.view)
    }

    private fun showPurchasePlans() {
        if (!PurchaseFlow.offered(SessionService.view.accountAccess?.projection) ||
            purchaseBusy() || PurchaseFlow.blocksNewPurchase(SessionService.view.purchase)) return
        val plans = PaymentsText.orderedPlans(SessionService.view.purchase?.plans.orEmpty())
        android.app.AlertDialog.Builder(this).setTitle("Тариф подписки")
            .setItems(plans.map(PaymentsText::purchasePlanLine).toTypedArray()) { _, index ->
                if (!purchaseBusy()) {
                    clearPurchaseSelection(clearPlan = true)
                    purchasePlanId = plans[index].planId
                    render(SessionService.view)
                    showPurchaseExtraSlots()
                }
            }
            .setNegativeButton(PaymentsText.BACK_TEXT) { _, _ -> clearPurchaseSelection(clearPlan = true) }
            .setOnCancelListener { clearPurchaseSelection(clearPlan = true) }
            .show()
    }

    /** Slot IDs belong to server-paid seats, never to the physical device list. */
    private fun showPurchaseExtraSlots() {
        val current = SessionService.view.purchase ?: return
        val plan = current.plans.firstOrNull { it.planId == purchasePlanId } ?: return
        val product = plan.product
        val slots = product?.extraSlots.orEmpty()
        if (product?.kind != "subscription" || slots.isEmpty()) {
            purchaseRenewExtraSlotIds = emptyList()
            showPurchaseMethods()
            return
        }
        val checked = BooleanArray(slots.size)
        val renewalTitle = TextView(this).apply {
            text = "Какие дополнительные места продлить?\n\n" + PaymentsText.EXTRA_RENEWAL_WARNING
            setPadding(24, 20, 24, 12)
        }
        android.app.AlertDialog.Builder(this)
            .setCustomTitle(renewalTitle)
            .setMultiChoiceItems(slots.map {
                PaymentsText.extraSlotLine(it, plan.currency, java.time.ZoneId.systemDefault())
            }.toTypedArray(), checked) { dialog, index, selected ->
                val allowed = selected && slots[index].renewAmountMinor != null
                checked[index] = allowed
                (dialog as android.app.AlertDialog).listView.setItemChecked(index, allowed)
            }
            .setPositiveButton("Далее") { _, _ ->
                if (!purchaseBusy()) {
                    purchaseRenewExtraSlotIds = slots.filterIndexed { index, _ -> checked[index] }
                        .map { it.slotId }.sorted()
                    render(SessionService.view)
                    showPurchaseMethods()
                }
            }
            .setNegativeButton(PaymentsText.BACK_TEXT) { _, _ -> clearPurchaseSelection(clearPlan = true) }
            .setOnCancelListener { clearPurchaseSelection(clearPlan = true) }
            .show()
    }

    private fun showPurchaseMethods() {
        val current = SessionService.view.purchase ?: return
        val plan = current.plans.firstOrNull { it.planId == purchasePlanId } ?: return
        if (!PurchaseFlow.offered(SessionService.view.accountAccess?.projection) ||
            purchaseBusy() || PurchaseFlow.blocksNewPurchase(current)) return
        val methods = PaymentsText.orderedMethods(plan)
        android.app.AlertDialog.Builder(this).setTitle("Способ оплаты")
            .setItems(methods.map { PaymentsText.methodLabel(it).orEmpty() }.toTypedArray()) { _, index ->
                if (!purchaseBusy()) {
                    clearPurchaseSelection(clearPlan = false)
                    purchaseMethod = methods[index]
                    purchaseQuoteAfterConfirmation = SessionService.view.purchase?.phase == PurchaseFlow.CONFIRMED
                    // Exactly one quote per explicit method choice. No render/resume retry.
                    startForegroundService(Intent(this, SessionService::class.java)
                        .setAction("purchase_quote")
                        .putExtra("plan_id", plan.planId)
                        .putExtra("duration_code", plan.durationCode)
                        .putExtra("method", purchaseMethod)
                        .putStringArrayListExtra("renew_extra_slot_ids", ArrayList(purchaseRenewExtraSlotIds)))
                    render(SessionService.view)
                }
            }
            .setNegativeButton(PaymentsText.CANCEL_TEXT) { _, _ -> clearPurchaseSelection(clearPlan = false) }
            .setOnCancelListener { clearPurchaseSelection(clearPlan = false) }
            .show()
    }

    /** Rendering cannot request a quote or create an invoice; it only displays server data. */
    private fun renderPurchase(state: ViewState) {
        val offered = PurchaseFlow.offered(state.accountAccess?.projection)
        val plans = PaymentsText.orderedPlans(state.purchase?.plans.orEmpty())
        val sending = state.purchase?.sending == true
        val bindingPaid = PurchaseFlow.blocksNewPurchase(state.purchase)
        val plan = plans.firstOrNull { it.planId == purchasePlanId }
        if (plan == null) purchasePlanId = null
        if (plan == null || purchaseMethod !in plan.methods) purchaseMethod = null
        val quote = selectedPurchaseQuote(state.purchase)
        purchasePlansButton.visibility = if (offered && plans.isEmpty() && !bindingPaid) View.VISIBLE else View.GONE
        purchasePlansButton.isEnabled = !sending
        purchasePlanButton.visibility = if (offered && plans.isNotEmpty() && !bindingPaid) View.VISIBLE else View.GONE
        purchasePlanButton.isEnabled = !sending
        purchasePlanButton.text = plan?.let {
            PaymentsText.purchasePlanLine(it) + if (it.product?.kind == "subscription")
                "\nБазовые места: ${it.baseDeviceLimit}. Продлить дополнительных: ${purchaseRenewExtraSlotIds.size}.\nОстальные сохранят прежние сроки."
            else ""
        } ?: "Выбрать тариф"
        purchaseMethodButton.visibility = if (offered && plan != null && !bindingPaid) View.VISIBLE else View.GONE
        purchaseMethodButton.isEnabled = !sending
        purchaseMethodButton.text = purchaseMethod?.let(PaymentsText::methodLabel) ?: "Выбрать способ оплаты"
        purchaseQuoteLine.visibility = if (offered && PurchaseVisibility.quoteLineVisible(quote, bindingPaid)) View.VISIBLE else View.GONE
        purchaseQuoteLine.text = quote?.let {
            (if (state.purchase?.quotePriceChanged == true) "Сервер уточнил цену. Новое предложение:\n" else "") +
                PaymentsText.quoteLine(it, java.time.ZoneId.systemDefault()) +
                (if (it.product?.kind == "subscription" &&
                    it.product.renewExtraSlotIds.size < it.product.extraSlots.size)
                    "\n" + PaymentsText.EXTRA_RENEWAL_WARNING else "")
        }.orEmpty()
        purchasePayButton.visibility = if (offered && PurchaseVisibility.payVisible(quote, bindingPaid)) View.VISIBLE else View.GONE
        displayedPurchaseQuote = quote.takeIf { offered && !sending && !bindingPaid }
        purchasePayButton.isEnabled = displayedPurchaseQuote != null

        if (!offered || state.purchase?.phase in setOf(PurchaseFlow.ERROR, PurchaseFlow.UNAVAILABLE,
                PurchaseFlow.EXPIRED_NO_ORDER, PurchaseFlow.NO_ORDER)) {
            checkoutOpenPolicy.clearAwaiting()
        }
        val open = if (offered) checkoutOpenPolicy.autoOpenAfterPay(state.purchase?.createAck) else null
        if (open != null) launchCheckoutBrowser(open)
        if (state.purchase?.phase != PurchaseFlow.CONFIRMED) purchaseQuoteAfterConfirmation = false
        purchaseStatus.text = purchaseStatusLine(state)
        PaymentsText.quoteHint(state.purchase, offered && purchaseMethod != null, quote != null,
            purchaseQuoteAfterConfirmation)?.let { purchaseStatus.append("\n$it") }
        // These actions depend on the existing order, not on the new choice or its cancellation.
        val payment = state.purchase?.payment
        purchaseContinueButton.visibility = if (checkoutOpenPolicy.canContinue(payment)) View.VISIBLE else View.GONE
        val recovery = state.purchase?.recovery
        purchaseCheckButton.visibility = if (payment != null || recovery != null) View.VISIBLE else View.GONE
        purchaseCheckButton.text = if (payment == null && recovery != null) "Восстановить оплату" else "Проверить статус оплаты"
        purchaseCheckButton.isEnabled = !sending && recovery !in setOf("legacy_unknown", "unreadable")
        purchaseReference.text = payment?.let { PaymentsText.checkoutReferenceText(it) }.orEmpty()
        purchaseReference.visibility = if (offered && purchaseReference.text.isNotEmpty()) View.VISIBLE else View.GONE
    }

    /** The accepted purchase status plus the visible checkout-open error, when one exists. */
    private fun purchaseStatusLine(state: ViewState): String = PaymentsText.purchaseStatus(state.purchase, state.accountAccess?.projection, java.time.ZoneId.systemDefault()) +
        (checkoutOpenPolicy.openError?.let { "\n$it" } ?: "")

    /**
     * The existing main-screen status text plus the same display-only access line as the
     * subscription surface ([AccountAccessPolicy]). No new permissions, transport or
     * admission wording is introduced.
     */
    private fun mainStatusText(state: ViewState): String = listOfNotNull(
        state.catalogStage?.label ?: UserStatusText.phase(state.phase),
        UserStatusText.error(if (consentDenied) "VPN_PERMISSION_DENIED" else state.error),
        pendingSelectionStatus(state),
        state.accountAccess?.let {
            AccountAccessPolicy.statusLine(it, android.os.SystemClock.elapsedRealtime())
        },
        // Owner warning shown before the first hour can start; display-only, no new flow.
        PreAdmissionConnect.HOUR_WARNING.takeIf { PreAdmissionConnect.eligible(state) },
        state.recoveryStatus?.let(RecoveryCodeUi::textForStatus),
    ).joinToString("\n")

    private fun pendingSelectionStatus(state: ViewState): String? {
        val selectable = state.displayMode != CatalogDisplayMode.BROWSE &&
            state.phase in setOf("CatalogReady", "Connected", "SwitchingServer", "KillSwitch")
        val pending = pendingChoiceId ?: state.pendingNodeId
        return when {
            !selectable -> null
            pending != null -> "Сохраняем выбор сервера…"
            NodeSelection.connectableNodeId(state.nodes, state.selectedNodeId) != null -> null
            state.selectedNodeId.isNotEmpty() -> "Сохранённый сервер недоступен. Выберите доступный сервер."
            else -> "Выберите сервер для подключения."
        }
    }

    private companion object {
        const val THEME_SYSTEM = 200; const val THEME_LIGHT = 201; const val THEME_DARK = 202
        const val SCHEDULE_OFF = 300; const val SCHEDULE_TWICE = 301
        const val SCHEDULE_DAILY = 302; const val SCHEDULE_WEEKLY = 303
        const val STATE_VISIBLE_TAB = "visible_tab"
        const val REQUEST_AUTOCONNECT_CONSENT = 101
        const val STATE_AUTOCONNECT_GENERATION = "autoconnect_generation"
        const val STATE_AUTOCONNECT_CONSENT_PENDING = "autoconnect_consent_pending"
    }
}

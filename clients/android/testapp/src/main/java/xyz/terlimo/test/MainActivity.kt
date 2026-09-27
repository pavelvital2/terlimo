package xyz.terlimo.test

import android.Manifest
import android.app.Activity
import android.content.res.ColorStateList
import android.content.ClipboardManager
import android.content.Intent
import android.content.pm.PackageManager
import android.net.VpnService
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.text.InputType
import android.view.View
import android.widget.*
import com.google.zxing.integration.android.IntentIntegrator

/** Product UI; no admin/deploy controls or secret diagnostics. */
class MainActivity : Activity() {
    private lateinit var link: EditText
    private lateinit var status: TextView
    private lateinit var nodes: Spinner
    private lateinit var importButton: Button
    private lateinit var connectButton: Button
    private lateinit var activateHourButton: Button
    private lateinit var registerTelegramButton: Button
    private lateinit var loginTelegramButton: Button
    private lateinit var trialButton: Button
    private lateinit var resumeButton: Button
    private lateinit var captchaButton: Button
    private lateinit var refreshButton: Button
    private lateinit var subscription: TextView
    private lateinit var subscriptionTerm: TextView
    private lateinit var subscriptionDevices: TextView
    private lateinit var subscriptionPlan: TextView
    private lateinit var devicesBlock: LinearLayout
    private lateinit var devicesRefreshButton: Button
    private lateinit var devicesCount: TextView
    private lateinit var purchasePlansButton: Button
    private lateinit var purchasePlanSpinner: Spinner
    private lateinit var purchaseMethodSpinner: Spinner
    private lateinit var purchaseQuoteLine: TextView
    private lateinit var purchaseQuoteButton: Button
    private lateinit var purchasePayButton: Button
    private lateinit var purchaseContinueButton: Button
    private lateinit var purchaseCheckButton: Button
    private lateinit var purchaseStatus: TextView
    private lateinit var purchaseReference: TextView
    private var purchasePlansShown: List<PaymentPlan> = emptyList()
    private var purchaseMethodChoices: List<String> = emptyList()
    private var purchasePlanId: String? = null
    private var purchaseMethod: String? = null
    private var purchaseRendering = false
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
    private var externalIntentConsumed = false
    // S5 §11 announcements display (Help tab). Values are rendered from ViewState only.
    private lateinit var announcementsBadge: TextView
    private lateinit var announcementsDenied: TextView
    private lateinit var announcementsEmpty: TextView
    private lateinit var announcementsList: LinearLayout
    private var openHelp: (() -> Unit)? = null
    private var helpTabButton: Button? = null
    private var openTab: ((NavTarget) -> Unit)? = null
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
        super.onCreate(savedInstanceState)
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
        externalIntentConsumed = savedInstanceState?.getBoolean(STATE_EXTERNAL_INTENT_CONSUMED) == true
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
                text = "Диагностика проверяет VPN, интернет, DNS и доступность соединения. Отчёт не содержит ключей и ссылки подписки."
                setPadding(0, 12, 0, 16)
            })
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
                addView(TextView(this@MainActivity).apply { text = "Уведомления"; textSize = 20f },
                    LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f))
                addView(announcementsBadge)
            })
            addView(announcementsDenied)
            addView(announcementsEmpty)
            addView(announcementsList)
        }
        subscriptionPanel.addView(TextView(this).apply { text = "Подписка TERLIMO"; textSize = 24f })
        link = EditText(this).apply {
            hint = "Подписанная ссылка whitelists://"
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
            importantForAutofill = View.IMPORTANT_FOR_AUTOFILL_NO_EXCLUDE_DESCENDANTS
            isSaveEnabled = false
            maxLines = 4
        }
        subscriptionPanel.addView(link)
        subscriptionPanel.addView(LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            addView(Button(this@MainActivity).apply {
                text = "Вставить"
                setOnClickListener {
                    val clipboard = getSystemService(ClipboardManager::class.java)
                    val value = clipboard?.primaryClip?.takeIf { it.itemCount > 0 }
                        ?.getItemAt(0)?.coerceToText(this@MainActivity)?.toString().orEmpty()
                    acceptImportInput(value)
                }
            })
            addView(Button(this@MainActivity).apply {
                text = "Сканировать QR"
                setOnClickListener {
                    IntentIntegrator(this@MainActivity)
                        .setCaptureActivity(QrCaptureActivity::class.java)
                        .setDesiredBarcodeFormats(IntentIntegrator.QR_CODE)
                        .setPrompt("Наведите камеру на QR подписки TERLIMO")
                        .setBeepEnabled(false)
                        .initiateScan()
                }
            })
            addView(Button(this@MainActivity).apply {
                text = "QR из файла"
                setOnClickListener {
                    startActivityForResult(Intent(Intent.ACTION_OPEN_DOCUMENT).apply {
                        addCategory(Intent.CATEGORY_OPENABLE)
                        type = "image/*"
                    }, REQUEST_QR_IMAGE)
                }
            })
        })
        subscriptionPanel.addView(Button(this).apply {
            text = "Открыть файл подписки"
            setOnClickListener {
                startActivityForResult(Intent(Intent.ACTION_OPEN_DOCUMENT).apply {
                    addCategory(Intent.CATEGORY_OPENABLE)
                    type = "*/*"
                    putExtra(Intent.EXTRA_MIME_TYPES, arrayOf("text/plain", "application/octet-stream"))
                }, REQUEST_SUBSCRIPTION_FILE)
            }
        })
        importButton = Button(this).apply {
            text = "Импортировать и получить каталог"
            setOnClickListener {
                acceptImportInput(link.text.toString())
            }
        }
        subscriptionPanel.addView(importButton)
        resumeButton = Button(this).apply {
            text = "Открыть сохранённую подписку"
            setOnClickListener { startForegroundService(Intent(this@MainActivity, SessionService::class.java).setAction("resume")) }
        }
        subscriptionPanel.addView(resumeButton)
        subscriptionPanel.addView(Button(this).apply {
            text = "Заменить подписку"
            contentDescription = "Удалить текущую подписку и подготовить импорт новой"
            setOnClickListener {
                val expectedState = SessionService.view
                if (expectedState.phase !in setOf("Idle", "Error")) {
                    status.text = "Сначала завершите текущее действие"
                    return@setOnClickListener
                }
                android.app.AlertDialog.Builder(this@MainActivity)
                    .setTitle("Заменить подписку?")
                    .setMessage("Текущая подписка, каталог и выбор сервера будут удалены. Ключ этой установки и настройки маршрутизации сохранятся.")
                    .setNegativeButton("Отмена", null)
                    .setPositiveButton("Удалить и заменить") { _, _ ->
                        runCatching {
                            check(SessionService.replaceSubscriptionIfUnchanged(expectedState) {
                                InstallationStore(this@MainActivity).replaceSubscription()
                            }) { "ACTION_IN_PROGRESS" }
                            pendingChoiceId = null
                            selectedForConsent = null
                            link.text.clear()
                            render(SessionService.view)
                            status.text = "Текущая подписка удалена. Откройте файл или вставьте новую подписанную ссылку."
                        }.onFailure {
                            status.text = if (it.message == "IMPORT_REQUIRED") "Сохранённая подписка не найдена"
                            else if (it.message == "ACTION_IN_PROGRESS") "Состояние изменилось. Повторите замену после завершения действия"
                            else "Не удалось заменить подписку"
                        }
                    }.show()
            }
        })
        refreshButton = Button(this).apply {
            text = "Обновить сохранённую подписку"
            visibility = View.GONE // catalog app bar owns the visible refresh action
            setOnClickListener { startForegroundService(Intent(this@MainActivity, SessionService::class.java).setAction("refresh")) }
        }
        subscriptionPanel.addView(refreshButton)
        subscriptionPanel.addView(TextView(this).apply {
            text = "Обновление вручную доступно после отключения. Во время VPN разрешение продлевается автоматически. После ошибки продолжайте сохранённую подписку — не удаляйте профиль."
        })
        subscription = TextView(this).apply { setPadding(0, 16, 0, 16); visibility = View.VISIBLE }
        subscriptionPanel.addView(subscription)
        subscriptionTerm = TextView(this).apply { setPadding(0, 0, 0, 8); visibility = View.GONE }
        subscriptionPanel.addView(subscriptionTerm)
        subscriptionDevices = TextView(this).apply { setPadding(0, 0, 0, 8); visibility = View.GONE }
        subscriptionPanel.addView(subscriptionDevices)
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
        purchasePlanSpinner = Spinner(this).apply {
            visibility = View.GONE
            contentDescription = "Тариф подписки"
            onItemSelectedListener = object : AdapterView.OnItemSelectedListener {
                override fun onNothingSelected(parent: AdapterView<*>?) = Unit
                override fun onItemSelected(parent: AdapterView<*>?, view: View?, position: Int, id: Long) {
                    if (purchaseRendering) return
                    val plan = purchasePlansShown.getOrNull(position) ?: return
                    if (plan.planId == purchasePlanId) return
                    purchasePlanId = plan.planId
                    purchaseMethod = plan.methods.firstOrNull()
                    render(SessionService.view)
                }
            }
        }
        subscriptionPanel.addView(purchasePlanSpinner)
        purchaseMethodSpinner = Spinner(this).apply {
            visibility = View.GONE
            contentDescription = "Способ оплаты"
            onItemSelectedListener = object : AdapterView.OnItemSelectedListener {
                override fun onNothingSelected(parent: AdapterView<*>?) = Unit
                override fun onItemSelected(parent: AdapterView<*>?, view: View?, position: Int, id: Long) {
                    if (purchaseRendering) return
                    val method = purchaseMethodChoices.getOrNull(position) ?: return
                    if (method == purchaseMethod) return
                    purchaseMethod = method
                    render(SessionService.view)
                }
            }
        }
        subscriptionPanel.addView(purchaseMethodSpinner)
        purchaseQuoteLine = TextView(this).apply { visibility = View.GONE }
        subscriptionPanel.addView(purchaseQuoteLine)
        purchaseQuoteButton = Button(this).apply {
            text = "Получить предложение"
            visibility = View.GONE
            setOnClickListener {
                val plan = purchasePlansShown.firstOrNull { it.planId == purchasePlanId } ?: return@setOnClickListener
                val method = purchaseMethod ?: return@setOnClickListener
                startForegroundService(Intent(this@MainActivity, SessionService::class.java)
                    .setAction("purchase_quote")
                    .putExtra("plan_id", plan.planId)
                    .putExtra("duration_code", plan.durationCode)
                    .putExtra("method", method))
            }
        }
        subscriptionPanel.addView(purchaseQuoteButton)
        purchasePayButton = Button(this).apply {
            text = "Оплатить"
            visibility = View.GONE
            setOnClickListener {
                val state = SessionService.view.purchase ?: return@setOnClickListener
                val quote = state.quote ?: return@setOnClickListener
                val plan = purchasePlansShown.firstOrNull { it.planId == purchasePlanId }
                    ?: return@setOnClickListener
                if (state.phase == PurchaseFlow.UNAVAILABLE || state.selectedPlanId != plan.planId ||
                    state.selectedMethod != purchaseMethod || quote.method != purchaseMethod ||
                    quote.durationCode != plan.durationCode) return@setOnClickListener
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
                if (PreAdmissionConnect.connectable(state, pendingChoiceId != null)) {
                    requestPreAdmissionConsent(state)
                } else if (state.phase in setOf("Connected", "Starting", "BootstrapConnecting", "NodeAuthenticating", "ConfiguringVPN", "SwitchingServer", "WaitingUser", "Reconnecting", "SleepPaused", "KillSwitch")) {
                    selectedForConsent = null
                    startService(Intent(this, SessionService::class.java).setAction("cancel"))
                } else requestVpnConsentForCurrentSelection()
            },
            onRefresh = { startForegroundService(Intent(this, SessionService::class.java).setAction("refresh")) },
        )
        mainPanel.addView(orbitHeader)
        activateHourButton = Button(this).apply {
            text = "Активировать 1 час для регистрации"
            visibility = View.GONE
            setOnClickListener { requestPreAdmissionConsent(projectedState()) }
        }
        mainPanel.addView(activateHourButton)
        registerTelegramButton = Button(this).apply {
            text = "Зарегистрироваться в Telegram"
            visibility = View.GONE
            setOnClickListener {
                startForegroundService(Intent(this@MainActivity, SessionService::class.java)
                    .setAction("telegram_register"))
            }
        }
        mainPanel.addView(registerTelegramButton)
        loginTelegramButton = Button(this).apply {
            text = "Уже есть аккаунт? Войти через Telegram"
            visibility = View.GONE
            setOnClickListener {
                startForegroundService(Intent(this@MainActivity, SessionService::class.java)
                    .setAction("telegram_login"))
            }
        }
        mainPanel.addView(loginTelegramButton)
        trialButton = Button(this).apply {
            text = "Получить 7 дней"
            visibility = View.GONE
            setOnClickListener {
                startForegroundService(Intent(this@MainActivity, SessionService::class.java)
                    .setAction("trial_activate"))
            }
        }
        mainPanel.addView(trialButton)
        catalogView = ServerCatalogView(this,
            onRefresh = { startForegroundService(Intent(this, SessionService::class.java).setAction("refresh")) },
            onSelect = { requestNodeSelection(it) },
            onProbe = { nodeId -> startService(Intent(this, SessionService::class.java).setAction("probe").putExtra("node_id", nodeId)) },
            onProbeAll = { startService(Intent(this, SessionService::class.java).setAction("probe_all")) },
            onProbeAllCancel = { startService(Intent(this, SessionService::class.java).setAction("probe_all_cancel")) },
            showChrome = false,
        )
        mainPanel.addView(catalogView)
        subscriptionPanel.addView(Button(this).apply {
            text = "Маршрутизация приложений"
            minHeight = 48
            setOnClickListener { startActivity(Intent(this@MainActivity, RoutingSettingsActivity::class.java)) }
        })
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
            setOnClickListener { if (SessionService.captcha != null) startActivity(Intent(this@MainActivity, CaptchaActivity::class.java)) }
        }
        mainPanel.addView(captchaButton)
        mainPanel.addView(Button(this).apply {
            text = "Отменить / отключить"
            visibility = View.GONE
            setOnClickListener {
                selectedForConsent = null
                startService(Intent(this@MainActivity, SessionService::class.java).setAction("cancel"))
            }
        })
        status = TextView(this).apply { textSize = 18f; setPadding(0, 24, 0, 0) }
        mainPanel.addView(status)
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
        val helpSurface = ScrollView(this).apply {
            isFillViewport = true
            setBackgroundColor(TerlimoCatalogBrandTokens.BACKGROUND.toInt())
            visibility = View.GONE
            helpPanel.visibility = View.VISIBLE
            addView(helpPanel)
        }
        // S5 §07.1: the Settings tab is its own real surface. It currently exposes only the
        // already-working retention entry; more settings topics are later slices (no stubs).
        val settingsPanel = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(32, 32, 32, 48)
            addView(TextView(this@MainActivity).apply { text = "Настройки"; textSize = 24f })
            addView(Button(this@MainActivity).apply {
                text = "Работа с выключенным экраном"
                minHeight = dp(48)
                setOnClickListener { startActivity(Intent(this@MainActivity, RetentionSettingsActivity::class.java)) }
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
            val surfaces = listOf(mainSurface, subscriptionSurface, settingsSurface, helpSurface)
            fun showSurface(visible: View) = surfaces.forEach {
                it.visibility = if (it === visible) View.VISIBLE else View.GONE
            }
            var navState = BottomNavigation.initial()
            fun onTab(target: NavTarget) {
                navState = BottomNavigation.onTap(navState, target)
                val visibleView = when (navState.visible) {
                    NavTarget.HOME -> mainSurface
                    NavTarget.SUBSCRIPTION -> subscriptionSurface
                    NavTarget.SETTINGS -> settingsSurface
                    NavTarget.HELP -> helpSurface
                    NavTarget.ROUTING -> mainSurface
                }
                showSurface(visibleView)
                destinationButtons.forEach { (tab, button) ->
                    button.setBottomNavigationActive(tab == navState.selected)
                }
                if (navState.visible != NavTarget.HOME) {
                    // Leaving the catalog screen must stop an in-flight common ping.
                    startService(Intent(this@MainActivity, SessionService::class.java).setAction("probe_all_cancel"))
                }
                if (target == NavTarget.ROUTING) {
                    startActivity(Intent(this@MainActivity, RoutingSettingsActivity::class.java))
                }
                if (target == NavTarget.HELP) {
                    // §11: opening the section asks the existing attempt for a fresh list.
                    startService(Intent(this@MainActivity, SessionService::class.java).setAction("announcements_request"))
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
            onTab(NavTarget.HOME)
        }
        val content = FrameLayout(this).apply {
            addView(mainSurface, FrameLayout.LayoutParams(FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT))
            addView(subscriptionSurface, FrameLayout.LayoutParams(FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT))
            addView(helpSurface, FrameLayout.LayoutParams(FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT))
            addView(settingsSurface, FrameLayout.LayoutParams(FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT))
        }
        setContentView(LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setBackgroundColor(TerlimoCatalogBrandTokens.BACKGROUND.toInt())
            addView(content, LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, 0, 1f))
            addView(bottomNavigation, LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT))
        })
        val launchIntent = intent
        val incomingImport = !externalIntentConsumed && launchIntent != null &&
            (launchIntent.action == Intent.ACTION_VIEW || launchIntent.action == Intent.ACTION_SEND) &&
            (launchIntent.dataString != null || launchIntent.getStringExtra(Intent.EXTRA_TEXT) != null)
        render(SessionService.view)
        handleIncomingIntent(intent)
        maybeOpenAnnouncements(intent)
        if (!incomingImport) autoLoadSavedSubscription()
    }

    /**
     * Owner contract 4: the first render of the process starts the standard linkless
     * bootstrap/resume through the existing seed gate, so /me and the gateways load
     * without tapping «Открыть сохранённую подписку». Exactly once per process; the
     * cached list never cancels the refresh (existing `resume` action unchanged).
     */
    private fun autoLoadSavedSubscription() {
        if (!AutoLoadPolicy.shouldStart(projectedState().nodes)) return
        if (!ProcessAutoLoad.claim()) return
        startForegroundService(Intent(this, SessionService::class.java).setAction("resume"))
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
        for (announcement in visible) {
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
            announcementsList.addView(row)
        }
    }

    /** Live view, or the durable retained catalog projected for a fresh process. */
    private fun projectedState(): ViewState =
        RetainedProjection.hydrate(SessionService.view,
            runCatching { installationStore.readCatalogCache() }.getOrNull())

    private fun requestVpnConsentForCurrentSelection() {
        val state = projectedState()
        val nodeId = NodeSelection.connectableNodeId(
            BrowseCatalogCodec.verifiedNodes(state), state.selectedNodeId)
        if (nodeId == null) {
            // Waiting for explicit first connect: no node selection exists yet. The
            // eligible /me (data_access=none, onboarding not_started) may still start
            // the first-connect consent; the separate explicit action follows consent.
            requestPreAdmissionConsent(state)
            return
        }
        val ready = state.phase == "CatalogReady"
        val retained = RetainedCatalogPolicy.connectableId(state) == nodeId
        if ((!ready && !retained) || pendingChoiceId != null || state.pendingNodeId != null) return
        consentDenied = false
        selectedForConsent = nodeId
        val consent = VpnService.prepare(this)
        if (consent != null) startActivityForResult(consent, 100) else connectSelected()
    }

    /**
     * Consent gate of the pre-admission first connect. It only starts the mandatory
     * VPN consent for the eligible waiting state; the explicit native command is sent
     * only from [connectSelected] after a granted consent.
     */
    private fun requestPreAdmissionConsent(state: ViewState) {
        if (!PreAdmissionConnect.connectable(state, pendingChoiceId != null)) return
        // Owner contract 5: with a non-empty browse list the connect waits for an explicit
        // row selection; the chosen gateway_id travels as the optional gateway_key.
        if (BrowseConnectGate.requiresSelection(state)) {
            status.text = "Выберите сервер из списка и нажмите подключение"
            return
        }
        consentDenied = false
        preAdmissionConsentRequest = true
        // Explicit owner warning before the first hour starts; display-only text.
        status.text = PreAdmissionConnect.HOUR_WARNING
        val consent = VpnService.prepare(this)
        if (consent != null) startActivityForResult(consent, 100) else connectSelected()
    }
    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        externalIntentConsumed = false
        setIntent(intent)
        handleIncomingIntent(intent)
        maybeOpenAnnouncements(intent)
    }
    override fun onSaveInstanceState(outState: Bundle) {
        outState.putBoolean(STATE_EXTERNAL_INTENT_CONSUMED, externalIntentConsumed)
        checkoutOpenPolicy.saveTo(outState)
        super.onSaveInstanceState(outState)
    }
    override fun onStart() {
        super.onStart()
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
        super.onStop()
    }

    /**
     * Returning from the external Telegram registration triggers exactly one server status
     * refresh while a registration is pending. No other lifecycle action is added.
     */
    override fun onResume() {
        super.onResume()
        if (SessionService.view.registration?.state == "pending") {
            startForegroundService(Intent(this, SessionService::class.java).setAction("telegram_refresh"))
        }
    }
    @Deprecated("Activity result used without adding an activity framework")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        val scan = IntentIntegrator.parseActivityResult(requestCode, resultCode, data)
        if (scan != null) {
            if (scan.contents != null) acceptImportInput(scan.contents)
            return
        }
        if (requestCode == REQUEST_QR_IMAGE) {
            if (resultCode == RESULT_OK && data?.data != null) decodeQrImage(data.data!!)
            return
        }
        if (requestCode == REQUEST_SUBSCRIPTION_FILE) {
            if (resultCode == RESULT_OK && data?.data != null) decodeSubscriptionFile(data.data!!)
            return
        }
        if (requestCode != 100) return
        if (resultCode == RESULT_OK) connectSelected()
        else {
            selectedForConsent = null
            preAdmissionConsentRequest = false
            consentDenied = true
            render(SessionService.view)
        }
    }
    private fun handleIncomingIntent(incoming: Intent?) {
        if (externalIntentConsumed) return
        val consumed = incoming ?: return
        val raw = when (consumed.action) {
            Intent.ACTION_VIEW -> consumed.dataString
            Intent.ACTION_SEND -> consumed.getStringExtra(Intent.EXTRA_TEXT)
            else -> null
        } ?: return
        externalIntentConsumed = true
        consumed.action = Intent.ACTION_MAIN
        consumed.data = null
        consumed.removeExtra(Intent.EXTRA_TEXT)
        acceptImportInput(raw)
    }
    private fun acceptImportInput(raw: String) {
        val value = runCatching { SubscriptionImportInput.normalize(raw) }.getOrElse {
            status.text = "Ссылка подписки не распознана"
            return
        }
        val state = SessionService.view
        if (state.phase != "Idle" && state.phase != "Error") {
            status.text = "Сначала завершите текущее действие"
            return
        }
        startForegroundService(Intent(this, SessionService::class.java)
            .setAction("import").putExtra("link", value))
        link.text.clear()
    }
    private fun decodeQrImage(uri: Uri) {
        Thread {
            val value = runCatching { QrImageDecoder.decode(contentResolver, uri) }
            runOnUiThread {
                value.onSuccess { acceptImportInput(it) }
                    .onFailure { status.text = "QR подписки не найден в изображении" }
            }
        }.start()
    }
    private fun decodeSubscriptionFile(uri: Uri) {
        Thread {
            val value = runCatching { SubscriptionTextFileReader.read(contentResolver, uri) }
            runOnUiThread {
                value.onSuccess { acceptImportInput(it) }
                    .onFailure { status.text = "Файл подписки повреждён или слишком большой" }
            }
        }.start()
    }
    private fun connectSelected() {
        if (preAdmissionConsentRequest) {
            // The separate explicit action after consent: no node id, no catalogue.
            // SessionService passes exactly this flag through the bridge to the runner.
            // The optional browsed gateway_key selects the explicit connect target.
            preAdmissionConsentRequest = false
            // Owner contract 5: re-check the current browse selection after the consent
            // round-trip; without a current selection the keyless fallback is refused.
            if (BrowseConnectGate.requiresSelection(SessionService.view)) {
                status.text = "Выберите сервер из списка и повторите подключение"
                render(SessionService.view)
                return
            }
            val gatewayKey = BrowseCatalogCodec.selectedId(SessionService.view)
            val intent = Intent(this, SessionService::class.java).setAction("onboarding_connect")
            if (gatewayKey.isNotEmpty()) intent.putExtra("gateway_key", gatewayKey)
            startForegroundService(intent)
            return
        }
        val id = selectedForConsent ?: return
        selectedForConsent = null
        val state = projectedState()
        if (id != NodeSelection.connectableNodeId(state.nodes, state.selectedNodeId)) return
        when {
            state.phase == "CatalogReady" && pendingChoiceId == null && state.pendingNodeId == null ->
                startService(Intent(this, SessionService::class.java).setAction("select").putExtra("node_id", id))
            RetainedCatalogPolicy.connectableId(state.phase, state.nodes, state.selectedNodeId) == id ->
                // Cold start: the retained id starts a fresh native attempt (foreground
                // service is required by the OS for a cold started service).
                startForegroundService(Intent(this, SessionService::class.java).setAction("connect").putExtra("node_id", id))
        }
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
        val idle = state.phase == "Idle" || state.phase == "Error"
        importButton.isEnabled = idle
        resumeButton.isEnabled = idle
        refreshButton.isEnabled = idle
        val displayed = pending ?: NodeSelection.displayedNodeId(state.nodes, state.selectedNodeId)
        val verifiedConnectable = NodeSelection.connectableNodeId(
            BrowseCatalogCodec.verifiedNodes(state), state.selectedNodeId)
        connectButton.isEnabled = (state.phase == "CatalogReady" && pending == null && verifiedConnectable != null) ||
            (pending == null && RetainedCatalogPolicy.connectableId(state) != null) ||
            BrowseConnectGate.connectable(state, pending != null)
        nodes.isEnabled = state.phase in setOf("CatalogReady", "Connected", "KillSwitch") && state.nodes.isNotEmpty()
        captchaButton.visibility = if (SessionService.captcha != null) View.VISIBLE else View.GONE
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
        activateHourButton.visibility = if (PreAdmissionConnect.connectable(state, pending != null))
            View.VISIBLE else View.GONE
        registerTelegramButton.visibility = if (RegistrationUi.registerVisible(state)) View.VISIBLE else View.GONE
        registerTelegramButton.text = RegistrationUi.buttonText(state)
        loginTelegramButton.visibility = if (RegistrationUi.loginVisible(state)) View.VISIBLE else View.GONE
        trialButton.visibility = if (TrialUi.activateVisible(state)) View.VISIBLE else View.GONE
        trialButton.text = TrialUi.buttonText(state)
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
     * network / invalid link becomes an honest visible error and the kept payment stays
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

    /**
     * S5 purchase rendering on the existing subscription surface. It shows only accepted
     * server results: exact server plan titles, the exact duration labels, price from the
     * exact amount_minor+currency, the provider checkout reference verbatim when present,
     * and a truthful pending/unavailable state. It never fabricates a QR and never offers
     * a hosted checkout (the frozen contract carries neither).
     */
    private fun renderPurchase(state: ViewState) {
        val registration = state.accountAccess?.projection?.registration
        val offered = PurchaseFlow.offered(registration)
        purchaseStatus.text = purchaseStatusLine(state, registration)
        val plans = PurchaseFlow.selectablePlans(state.purchase?.plans.orEmpty())
        purchaseRendering = true
        // Single-flight: while one purchase request is outstanding, pay/selection are disabled
        // and the status line shows waiting; the service guard holds regardless of this UI.
        val sending = state.purchase?.sending == true
        purchasePlansButton.isEnabled = !sending
        purchasePlanSpinner.isEnabled = !sending
        purchaseMethodSpinner.isEnabled = !sending
        purchaseQuoteButton.isEnabled = !sending
        purchasePayButton.isEnabled = !sending
        try {
            if (!offered) {
                purchasePlansButton.visibility = View.GONE
                purchasePlanSpinner.visibility = View.GONE
                purchaseMethodSpinner.visibility = View.GONE
                purchaseQuoteLine.visibility = View.GONE
                purchaseQuoteButton.visibility = View.GONE
                purchasePayButton.visibility = View.GONE
                purchaseContinueButton.visibility = View.GONE
                purchaseCheckButton.visibility = View.GONE
                purchaseReference.visibility = View.GONE
                purchasePlansShown = emptyList()
                purchaseMethodChoices = emptyList()
                purchasePlanId = null
                purchaseMethod = null
                return
            }
            if (plans.map { it.planId } != purchasePlansShown.map { it.planId }) {
                purchasePlansShown = plans
                purchasePlanSpinner.adapter = ArrayAdapter(this, android.R.layout.simple_spinner_dropdown_item,
                    plans.map { PaymentsText.planLine(it) })
                purchasePlanId = plans.firstOrNull()?.planId
                purchaseMethod = plans.firstOrNull()?.methods?.firstOrNull()
            }
            if (plans.isEmpty()) {
                purchasePlansButton.visibility = View.VISIBLE
                purchasePlanSpinner.visibility = View.GONE
                purchaseMethodSpinner.visibility = View.GONE
                purchaseQuoteLine.visibility = View.GONE
                purchaseQuoteButton.visibility = View.GONE
                purchasePayButton.visibility = View.GONE
                purchaseContinueButton.visibility = View.GONE
                purchaseCheckButton.visibility = View.GONE
                purchaseReference.visibility = View.GONE
                return
            }
            purchasePlansButton.visibility = View.GONE
            purchasePlanSpinner.visibility = View.VISIBLE
            val planPosition = plans.indexOfFirst { it.planId == purchasePlanId }.coerceAtLeast(0)
            if (purchasePlanSpinner.selectedItemPosition != planPosition) purchasePlanSpinner.setSelection(planPosition, false)
            val plan = plans[planPosition]
            purchasePlanId = plan.planId
            if (plan.methods != purchaseMethodChoices) {
                purchaseMethodChoices = plan.methods
                purchaseMethodSpinner.adapter = ArrayAdapter(this, android.R.layout.simple_spinner_dropdown_item,
                    plan.methods.map { PaymentsText.methodLabel(it) ?: it })
            }
            if (purchaseMethod !in plan.methods) purchaseMethod = plan.methods.firstOrNull()
            val methodPosition = plan.methods.indexOf(purchaseMethod).coerceAtLeast(0)
            if (purchaseMethodSpinner.selectedItemPosition != methodPosition) purchaseMethodSpinner.setSelection(methodPosition, false)
            purchaseMethodSpinner.visibility = if (plan.methods.isEmpty()) View.GONE else View.VISIBLE

            val quote = state.purchase?.quote?.takeIf {
                state.purchase.phase != PurchaseFlow.UNAVAILABLE &&
                    state.purchase.selectedPlanId == plan.planId &&
                    state.purchase.selectedMethod == purchaseMethod &&
                    it.method == purchaseMethod && it.durationCode == plan.durationCode
            }
            purchaseQuoteLine.text = quote?.let {
                "Предложение: " + PaymentsText.quoteLine(it, java.time.ZoneId.systemDefault())
            }.orEmpty()
            // A paid order awaiting binding must not offer a second payment: the parked order
            // is already created and only Telegram registration can apply it (S5 §3.2C).
            val bindingPaid = PurchaseFlow.paidAwaitingBinding(state.purchase)
            purchaseQuoteLine.visibility =
                if (PurchaseVisibility.quoteLineVisible(quote, bindingPaid)) View.VISIBLE else View.GONE
            purchaseQuoteButton.visibility =
                if (PurchaseVisibility.quoteButtonVisible(quote, bindingPaid)) View.VISIBLE else View.GONE
            purchasePayButton.visibility =
                if (PurchaseVisibility.payVisible(quote, bindingPaid)) View.VISIBLE else View.GONE

            val payment = state.purchase?.payment
            // A create/attempt failure (the existing error/unavailable purchase phase) drops
            // the live marker: no later payment of a failed attempt may auto-open.
            if (state.purchase?.phase == PurchaseFlow.ERROR ||
                state.purchase?.phase == PurchaseFlow.UNAVAILABLE) {
                checkoutOpenPolicy.clearAwaiting()
            }
            // Only the live consequence of the explicit «Оплатить» tap may open the browser,
            // and only through the correlated create result of the exact sent quote; a
            // payment from ViewState (an old order), an ordinary render, a status refresh or
            // a recreation can never consume the marker.
            val open = checkoutOpenPolicy.autoOpenAfterPay(state.purchase?.createAck)
            if (open != null) launchCheckoutBrowser(open)
            // Repaint: a refused/invalid reference or a failed launch stores its visible error now.
            purchaseStatus.text = purchaseStatusLine(state, registration)
            purchaseContinueButton.visibility =
                if (checkoutOpenPolicy.canContinue(payment)) View.VISIBLE else View.GONE
            purchaseReference.text = payment?.let { PaymentsText.checkoutReferenceText(it) }.orEmpty()
            purchaseReference.visibility = if (purchaseReference.text.isNullOrEmpty()) View.GONE else View.VISIBLE
            purchaseCheckButton.visibility = if (payment == null) View.GONE else View.VISIBLE
        } finally {
            purchaseRendering = false
        }
    }

    /** The accepted purchase status plus the visible checkout-open error, when one exists. */
    private fun purchaseStatusLine(
        state: ViewState,
        registration: AccountAccessProjection.Registration?,
    ): String = PaymentsText.purchaseStatus(state.purchase, registration, java.time.ZoneId.systemDefault()) +
        (checkoutOpenPolicy.openError?.let { "\n$it" } ?: "")

    /**
     * The existing main-screen status text plus the same display-only access line as the
     * subscription surface ([AccountAccessPolicy]). No new permissions, transport or
     * admission wording is introduced.
     */
    private fun mainStatusText(state: ViewState): String = listOfNotNull(
        UserStatusText.phase(state.phase),
        UserStatusText.error(if (consentDenied) "VPN_PERMISSION_DENIED" else state.error),
        pendingSelectionStatus(state),
        state.accountAccess?.let {
            AccountAccessPolicy.statusLine(it, android.os.SystemClock.elapsedRealtime())
        },
        // Owner warning shown before the first hour can start; display-only, no new flow.
        PreAdmissionConnect.HOUR_WARNING.takeIf { PreAdmissionConnect.eligible(state) },
        RegistrationUi.statusText(state),
        TrialUi.statusText(state),
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
        const val STATE_EXTERNAL_INTENT_CONSUMED = "external_intent_consumed"
        const val REQUEST_QR_IMAGE = 201
        const val REQUEST_SUBSCRIPTION_FILE = 202
    }
}

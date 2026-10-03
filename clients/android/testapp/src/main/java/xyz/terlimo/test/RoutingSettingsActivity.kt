package xyz.terlimo.test

import android.app.Activity
import android.content.Intent
import android.content.pm.ApplicationInfo
import android.content.res.ColorStateList
import android.os.Build
import android.os.Bundle
import android.view.View
import android.view.WindowInsets
import android.widget.*

/** Local, secret-free routing editor. Saving is disabled while a session is active. */
class RoutingSettingsActivity : Activity() {
    override fun attachBaseContext(newBase: android.content.Context) {
        // §26.1: resolve the stored per-user theme before any view is created.
        super.attachBaseContext(AppTheme.wrap(newBase))
    }

    private val protected by lazy { setOf(packageName) }
    private val presets = QuickExclusionCatalog(listOf(QuickExclusionPreset(QUICK_PRESET_ID, 18, QUICK_PACKAGES)))
    private lateinit var store: InstallationStore
    private lateinit var appMode: RadioGroup
    private lateinit var appList: LinearLayout
    private val checked = linkedSetOf<String>()
    private var loaded: RoutingSettingsDocument? = null
    private var showSystem = false
    private var query = ""
    private lateinit var modeHint: TextView
    private var installed: Map<String, ApplicationInfo> = emptyMap()

    override fun onCreate(savedInstanceState: Bundle?) {
        setTheme(AppTheme.platformTheme())
        super.onCreate(savedInstanceState)
        store = InstallationStore(this)
        loaded = runCatching { store.readRoutingSettings(protected, presets) }.getOrNull()
        checked += loaded?.routing?.apps?.packageNames.orEmpty().filterNot { it in protected }
        installed = packageManager.getInstalledApplications(0).associateBy { it.packageName }

        val root = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        // Fixed header + display filter, outside the scrolling list.
        root.addView(TextView(this).apply {
            text = "Маршрутизация"; textSize = 24f
            setPadding(dp(32), dp(32), dp(32), 0)
        })
        root.addView(Button(this).apply {
            text = "Назад в настройки"; minHeight = dp(48)
            setPadding(dp(32), 0, dp(32), 0)
            setOnClickListener { finish() }
        })
        root.addView(Switch(this).apply {
            text = "Показать системные приложения"
            contentDescription = "Фильтр списка: показать системные приложения"
            isChecked = showSystem
            minHeight = dp(48)
            setPadding(dp(32), 0, dp(32), 0)
            setOnCheckedChangeListener { _, value -> showSystem = value; renderAppList() }
        })
        // Display-only search, outside the scrolling list: it filters rows and never mutates
        // the persisted selection, so a hidden checked app stays saved.
        root.addView(EditText(this).apply {
            hint = "Поиск приложений"
            contentDescription = "Поиск приложений по названию или пакету"
            minHeight = dp(48)
            isSingleLine = true
            setPadding(dp(32), 0, dp(32), 0)
            addTextChangedListener(object : android.text.TextWatcher {
                override fun afterTextChanged(s: android.text.Editable?) {
                    query = s?.toString().orEmpty(); renderAppList()
                }
                override fun beforeTextChanged(s: CharSequence?, start: Int, count: Int, after: Int) {}
                override fun onTextChanged(s: CharSequence?, start: Int, before: Int, count: Int) {}
            })
        })

        val content = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; setPadding(dp(32), 0, dp(32), dp(16)) }
        content.addView(TextView(this).apply { text = "Настройки применятся при следующем подключении. Активный туннель не изменяется." })
        content.addView(TextView(this).apply { text = "Приложения"; textSize = 18f })
        appMode = RadioGroup(this).apply {
            orientation = RadioGroup.VERTICAL
            addView(radio(APP_DISABLED, "Все через VPN"))
            addView(radio(APP_INCLUDE, "Выбранные через VPN"))
            addView(radio(APP_EXCLUDE, "Выбранные мимо VPN"))
            // Exactly one instruction, kept in sync with the selected mode.
            setOnCheckedChangeListener { _, id -> modeHint.text = RoutingModeHint.text(modeForChecked(id)) }
        }
        content.addView(appMode)
        modeHint = hint(RoutingModeHint.ALL)
        content.addView(modeHint)
        content.addView(Button(this).apply {
            text = "Выбрать приложения из белого списка"
            contentDescription = "Отметить установленные приложения из подготовленного списка"
            minHeight = dp(48)
            setOnClickListener { applyQuickExclusions() }
        })
        appList = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        content.addView(appList)
        content.addView(TextView(this).apply { text = "DNS выбирается автоматически из защищённой конфигурации выбранного сервера." })
        root.addView(ScrollView(this).apply { addView(content) }, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, 0, 1f))

        // Fixed in-app home navigation, outside the scrollable list and above the bottom
        // navigation inset (mirrors MainActivity's bottom-navigation "Главная" affordance).
        val homeBar = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            setBackgroundColor(TerlimoCatalogBrandTokens.BACKGROUND.toInt())
            setPadding(dp(8), dp(4), dp(8), dp(4))
            addView(Button(this@RoutingSettingsActivity).apply {
                text = "Главная"
                configureHomeButton(R.drawable.ic_nav_home)
                setOnClickListener {
                    startActivity(Intent(this@RoutingSettingsActivity, MainActivity::class.java)
                        .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP))
                    finish()
                }
            }, LinearLayout.LayoutParams(0, dp(64), 1f))
        }
        root.addView(homeBar)

        // Fixed bottom action bar: Save stays reachable however long the list scrolls.
        val bottomBar = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(32), dp(8), dp(32), dp(32))
            addView(Button(this@RoutingSettingsActivity).apply {
                text = "Сохранить"
                minimumHeight = dp(48)
                isEnabled = RoutingEditGate.allowsSave(SessionService.routingEditSnapshot())
                setOnClickListener { save() }
            })
        }
        root.addView(bottomBar)

        setContentView(root)
        applySystemInsets(root)
        renderCurrent()
    }

    private fun applySystemInsets(root: View) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            root.setOnApplyWindowInsetsListener { view, insets ->
                val bars = insets.getInsets(WindowInsets.Type.systemBars())
                view.setPadding(0, bars.top, 0, bars.bottom)
                insets
            }
        } else {
            @Suppress("DEPRECATION")
            root.setOnApplyWindowInsetsListener { view, insets ->
                view.setPadding(0, insets.systemWindowInsetTop, 0, insets.systemWindowInsetBottom)
                insets
            }
        }
    }

    private fun Button.configureHomeButton(icon: Int) {
        minWidth = 0
        minHeight = dp(48)
        isAllCaps = false
        textSize = 11f
        maxLines = 1
        ellipsize = null
        gravity = android.view.Gravity.CENTER
        background = null
        stateListAnimator = null
        elevation = 0f
        setCompoundDrawablesRelativeWithIntrinsicBounds(0, icon, 0, 0)
        compoundDrawablePadding = dp(2)
        setPadding(dp(2), dp(4), dp(2), dp(2))
        val color = TerlimoCatalogBrandTokens.ACCENT.toInt()
        setTextColor(color)
        compoundDrawableTintList = ColorStateList.valueOf(color)
    }

    private fun radio(id: Int, label: String) = RadioButton(this).apply { this.id = id; text = label; minHeight = dp(48) }

    private fun modeForChecked(id: Int): AppRoutingMode = when (id) {
        APP_INCLUDE -> AppRoutingMode.INCLUDE_ONLY
        APP_EXCLUDE -> AppRoutingMode.EXCLUDE
        else -> AppRoutingMode.DISABLED
    }

    private fun hint(text: String) = TextView(this).apply {
        this.text = text; textSize = 13f
        setTextColor(TerlimoCatalogBrandTokens.MUTED_TEXT.toInt())
        setPadding(dp(32), 0, dp(32), dp(4))
    }

    private fun renderCurrent() {
        val current = loaded
        val mode = current?.routing?.apps?.mode ?: AppRoutingMode.DISABLED
        appMode.check(when (mode) {
            AppRoutingMode.DISABLED -> APP_DISABLED
            AppRoutingMode.EXCLUDE -> APP_EXCLUDE
            AppRoutingMode.INCLUDE_ONLY -> APP_INCLUDE
        })
        // Initial render must show the hint of the saved mode, not a stale/default one.
        modeHint.text = RoutingModeHint.text(mode)
        renderAppList()
    }

    /**
     * Display-only rendering. `checked` is never mutated here, so hiding system rows
     * (filter off) cannot drop a persisted system-package selection.
     */
    private fun renderAppList() {
        appList.removeAllViews()
        val allNames = (installed.keys + checked).filterNot { it in protected }.sortedBy { name ->
            installed[name]?.loadLabel(packageManager)?.toString()?.lowercase() ?: name.lowercase()
        }
        allNames.forEach { packageName ->
            val info = installed[packageName]
            val system = info?.let { RoutingSystemFilter.isSystem(it.flags) } == true
            if (!RoutingSystemFilter.isVisible(system, showSystem)) return@forEach
            val label = info?.loadLabel(packageManager)?.toString().orEmpty()
            if (!RoutingSystemFilter.matchesDisplay(label, packageName, query)) return@forEach
            appList.addView(CheckBox(this).apply {
                tag = packageName
                text = when { info == null -> "$packageName · не установлено"; system -> "$label · системное"; else -> label }
                isChecked = packageName in checked
                minHeight = dp(48)
                setOnCheckedChangeListener { _, yes -> if (yes) checked += packageName else checked -= packageName }
            })
        }
    }

    private fun applyQuickExclusions() {
        // Select installed matching packages by state, independent of the display filter,
        // then re-render. Hidden selected system rows stay hidden but remain persisted.
        val added = RoutingSystemFilter.quickAdditions(installed.keys, QUICK_PACKAGES, checked)
        checked += added
        renderAppList()
        Toast.makeText(this, if (added.isEmpty()) "Подходящие приложения не найдены" else "Выбрано: ${added.size}", Toast.LENGTH_SHORT).show()
    }

    private fun save() {
        if (!RoutingEditGate.allowsSave(SessionService.routingEditSnapshot())) {
            Toast.makeText(this, "Сначала отключите VPN", Toast.LENGTH_LONG).show()
            return
        }
        val mode = when (appMode.checkedRadioButtonId) {
            APP_INCLUDE -> AppRoutingMode.INCLUDE_ONLY
            APP_EXCLUDE -> AppRoutingMode.EXCLUDE
            else -> AppRoutingMode.DISABLED
        }
        // Empty INCLUDE must not replace the previous policy with a fail-closed/empty allow-list.
        if (!RoutingSettingsApply.shouldPersist(mode, checked)) {
            Toast.makeText(this, "Выберите хотя бы одно приложение", Toast.LENGTH_LONG).show()
            return
        }
        val result = runCatching {
            val appPolicy = RoutingPolicyNormalizer.apps(mode,
                if (mode == AppRoutingMode.DISABLED) emptyList() else checked,
                emptyList(), protected, presets)
            val next = RoutingSettingsDocument(revision = (loaded?.revision ?: 0) + 1,
                routing = RoutingPolicy(appPolicy))
            check(store.compareAndSetRoutingSettings(loaded?.revision, next, protected, presets)) { "SETTINGS_CHANGED" }
            next
        }
        result.onSuccess { loaded = it; Toast.makeText(this, "Настройки сохранены", Toast.LENGTH_SHORT).show(); finish() }
            .onFailure { Toast.makeText(this, "Настройки не сохранены: проверьте значения", Toast.LENGTH_LONG).show() }
    }

    private fun dp(value: Int) = (value * resources.displayMetrics.density).toInt()

    private companion object {
        const val APP_DISABLED = 100; const val APP_EXCLUDE = 101; const val APP_INCLUDE = 102
        const val QUICK_PRESET_ID = "wdtt-v18-recommended"
        val QUICK_PACKAGES = setOf(
            "ru.rostel", "ru.gosuslugi.goskey", "com.octopod.russianpost.client.android", "ru.crptech.mark",
            "ru.sberbankmobile", "ru.tinkoff.mb", "ru.vtb24.mobilebanking.android", "ru.alfabank.mobile.android",
            "logo.com.mbanking", "ru.mts.bank", "ru.lewis.dbo", "ru.gazprombank.android.mobilebank.app",
            "ru.nspk.mirpay", "ru.nspk.sbpay", "com.vkontakte.android", "com.vk.calls", "com.vk.im",
            "com.vk.vkvideo", "ru.vk.music", "ru.ok.android", "ru.oneme.app", "ru.mail.mailapp", "ru.mail.cloud",
            "ru.vk.store", "ru.ozon.app.android", "com.wildberries.ru", "ru.beru.android", "ru.megamarket",
            "ru.sbermarket", "ru.instamart", "ru.samokat.android", "com.avito.android", "ru.domclick.mortgage",
            "ru.hh.android", "ru.yandex.searchplugin", "ru.yandex.yandexmaps", "ru.yandex.music", "ru.kinopoisk",
            "com.yandex.browser", "ru.yandex.taxi", "ru.mts", "ru.beeline.services", "ru.tele2.mytele2",
            "ru.megafon.mlk", "ru.vkusvill", "com.icemobile.lenta.prod", "ru.reksoft.okey", "ru.myauchan.droid",
            "www.metro.com", "ru.tander.magnit", "ru.pyaterochka.app.browser", "ru.perekrestok.app", "club.chizhik",
            "com.logistic.sdek", "ru.dodopizza.app", "ru.burgerking", "com.apegroup.mcdonaldsrussia",
            "ru.dublgis.dgismobile", "ru.rzd.pass", "ru.tutu.tutu_emp", "com.carshering", "youdrive.today",
            "com.belkacar", "ru.aeroflot", "com.taxsee.taxsee", "ru.maximoff.max", "ru.rutube.app",
            "ru.ivi.client", "ru.more.play", "ru.zen.android", "gpm.tnt_premier", "ru.mts.mtstv",
            "ru.radioplayer", "com.gismeteo.client",
        )
    }
}

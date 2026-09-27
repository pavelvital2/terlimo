package xyz.terlimo.test

import android.content.Context
import android.graphics.Typeface
import android.graphics.drawable.GradientDrawable
import android.graphics.drawable.LayerDrawable
import android.os.SystemClock
import android.view.Gravity
import android.view.View
import android.widget.ImageView
import android.widget.ImageButton
import android.widget.LinearLayout
import android.widget.TextView

/** Approved A "Orbit" product header. Catalog stays authoritative in ServerCatalogView. */
internal class OrbitHomeHeader(
    context: Context,
    private val onPower: () -> Unit,
    private val onRefresh: () -> Unit,
) : LinearLayout(context) {
    private val power: ImageButton
    private val title: TextView
    private val subtitle: TextView
    private val quality: TextView
    private val speed: TextView
    private val traffic: TextView
    private val usage: TextView

    init {
        orientation = VERTICAL
        setPadding(dp(18), dp(18), dp(18), dp(8))
        setBackgroundColor(TerlimoCatalogBrandTokens.BACKGROUND.toInt())
        addView(row(dp(64)).apply {
            addView(ImageView(context).apply {
                setImageResource(R.drawable.terlimo_vpn_logo)
                contentDescription = "Логотип TERLIMO"
            }, LayoutParams(dp(48), dp(48)))
            addView(column().apply {
                addView(label("TERLIMO", 20f, bold = true))
                addView(label("VPN-КЛИЕНТ", 11f, muted = true))
            }, LayoutParams(0, LayoutParams.WRAP_CONTENT, 1f).apply { marginStart = dp(12) })
            addView(ImageButton(context).apply {
                setImageResource(R.drawable.ic_refresh)
                contentDescription = "Обновить каталог серверов"
                setColorFilter(TerlimoCatalogBrandTokens.TEXT.toInt())
                setPadding(dp(12), dp(12), dp(12), dp(12))
                background = ring(TerlimoCatalogBrandTokens.DIVIDER.toInt(), dp(12), 1,
                    TerlimoCatalogBrandTokens.SURFACE.toInt())
                setOnClickListener { onRefresh() }
            }, LayoutParams(dp(48), dp(48)))
        })
        power = ImageButton(context).apply {
            setImageResource(R.drawable.ic_vpn_power_off)
            contentDescription = "Подключить VPN"
            scaleType = ImageView.ScaleType.CENTER_INSIDE
            setPadding(dp(58), dp(58), dp(58), dp(58))
            background = powerBackground(TerlimoCatalogBrandTokens.DIVIDER.toInt(), connected = false)
            setColorFilter(TerlimoCatalogBrandTokens.MUTED_TEXT.toInt())
            setOnClickListener { onPower() }
        }
        addView(power, LayoutParams(dp(160), dp(160)).apply { gravity = Gravity.CENTER_HORIZONTAL; topMargin = dp(24) })
        title = label("VPN выключен", 22f, bold = true).apply { gravity = Gravity.CENTER }
        subtitle = label("Выберите сервер и подключитесь", 15f, muted = true).apply { gravity = Gravity.CENTER }
        addView(title, section(dp(18)))
        addView(subtitle, section(dp(4)))
        addView(row().apply {
            quality = metric("КАЧЕСТВО"); addView(quality, weighted())
            speed = metric("СКОРОСТЬ ↓ / ↑"); addView(speed, weighted(dp(8)))
            traffic = metric("ТРАФИК ↓ / ↑"); addView(traffic, weighted(dp(8)))
        }, section(dp(22)))
        // Account credited totals (server truth) kept explicitly separate from the local
        // live counters above; unknown/stale/gap and complete=false are shown, never hidden.
        usage = label("Зачёт: —", 12f, muted = true)
        addView(usage, section(dp(4)))
        addView(label("Серверы", 20f, bold = true), section(dp(18)))
    }

    fun render(state: ViewState, pendingChoice: Boolean = false) {
        // Owner contract 5: whenever the current display is browse, the first connect needs a
        // current row selection (loading, empty and offline states included); until then the
        // power action is disabled with an honest hint.
        val pendingBrowseChoice = PreAdmissionConnect.connectable(state, pendingChoice) &&
            BrowseConnectGate.requiresSelection(state)
        val preAdmissionConnect = PreAdmissionConnect.connectable(state, pendingChoice) && !pendingBrowseChoice
        val connected = state.phase == "Connected"
        // KillSwitch is a protected hold, not a connect attempt: a fresh attempt may still be
        // started by choosing another gateway, so it must not show the shared "Подключение…".
        val blocked = state.phase == "KillSwitch"
        val connecting = !preAdmissionConnect && state.phase in setOf("Starting", "BootstrapConnecting", "NodeAuthenticating", "ConfiguringVPN", "SwitchingServer", "WaitingUser", "Reconnecting", "SleepPaused")
        title.text = when {
            connected -> "VPN подключён"
            state.phase == "Stopping" -> "Отключение…"
            state.phase == "SleepPaused" -> "Пауза для экономии батареи"
            blocked -> "Сеть заблокирована"
            connecting -> "Подключение…"
            else -> "VPN выключен"
        }
        subtitle.text = when {
            connected -> state.nodes.singleOrNull { it.id == state.selectedNodeId }?.name ?: "Выбранный сервер"
            blocked -> "Трафик заблокирован. Выберите другой сервер или отключите VPN."
            pendingBrowseChoice -> when {
                BrowseCatalogCodec.displayedNodes(state).isNotEmpty() -> "Выберите сервер из списка для подключения"
                state.browseError != null -> "Список серверов недоступен. Обновите список."
                state.browseLoaded -> "Список серверов пуст. Обновите список."
                else -> "Загружаем список серверов…"
            }
            preAdmissionConnect -> PreAdmissionConnect.HOUR_PURPOSE
            state.selectedNodeId.isBlank() -> "Выберите сервер и подключитесь"
            else -> state.nodes.singleOrNull { it.id == state.selectedNodeId }?.name ?: "Выберите доступный сервер"
        }
        power.contentDescription = if (connected || connecting || blocked) "Отключить VPN" else "Подключить VPN"
        power.setImageResource(when {
            connected -> R.drawable.ic_vpn_connected
            connecting -> R.drawable.ic_vpn_connecting
            else -> R.drawable.ic_vpn_power_off
        })
        val verifiedConnectable = NodeSelection.connectableNodeId(
            BrowseCatalogCodec.verifiedNodes(state), state.selectedNodeId)
        power.isEnabled = preAdmissionConnect || connected || connecting || blocked || verifiedConnectable != null
        val accent = if (connected) TerlimoCatalogBrandTokens.ACCENT.toInt() else TerlimoCatalogBrandTokens.DIVIDER.toInt()
        power.background = powerBackground(accent, connected)
        power.setColorFilter(if (connected) TerlimoCatalogBrandTokens.ACCENT.toInt() else TerlimoCatalogBrandTokens.MUTED_TEXT.toInt())
        quality.setMetricValue(ChannelsDisplay.line(connected, state.channels, state.wakeRecovery?.text) ?: when {
            state.phase == "SleepPaused" -> "Трафик заблокирован"
            blocked -> "Трафик заблокирован"
            connected -> "Подключено"
            else -> "Недоступно"
        })
        speed.setMetricValue(TrafficText.speedLine(state.traffic))
        traffic.setMetricValue(TrafficText.trafficLine(state.traffic))
        usage.text = ServerUsageText.line(state.serverUsage, SystemClock.elapsedRealtime(), state.serverUsageUnavailable)
    }

    /** Light update for a traffic-only state change: never a full screen rebuild. */
    fun setTraffic(snapshot: TrafficSnapshot) {
        speed.setMetricValue(TrafficText.speedLine(snapshot))
        traffic.setMetricValue(TrafficText.trafficLine(snapshot))
    }

    private fun metric(caption: String) = TextView(context).apply {
        tag = caption
        text = "$caption\n—"
        textSize = 12f
        setTextColor(TerlimoCatalogBrandTokens.TEXT.toInt())
        setPadding(dp(10), dp(10), dp(8), dp(10))
        minHeight = dp(62)
        background = ring(TerlimoCatalogBrandTokens.DIVIDER.toInt(), dp(14), 1, TerlimoCatalogBrandTokens.SURFACE.toInt())
    }
    private fun TextView.setMetricValue(value: String) { text = "${tag as String}\n$value" }
    private fun row(minHeight: Int = 0) = LinearLayout(context).apply { orientation = HORIZONTAL; gravity = Gravity.CENTER_VERTICAL; minimumHeight = minHeight }
    private fun column() = LinearLayout(context).apply { orientation = VERTICAL }
    private fun label(value: String, sp: Float, bold: Boolean = false, muted: Boolean = false) = TextView(context).apply {
        text = value; textSize = sp
        setTextColor(if (muted) TerlimoCatalogBrandTokens.MUTED_TEXT.toInt() else TerlimoCatalogBrandTokens.TEXT.toInt())
        if (bold) typeface = Typeface.DEFAULT_BOLD
    }
    private fun weighted(start: Int = 0) = LayoutParams(0, LayoutParams.WRAP_CONTENT, 1f).apply { marginStart = start }
    private fun section(top: Int) = LayoutParams(LayoutParams.MATCH_PARENT, LayoutParams.WRAP_CONTENT).apply { topMargin = top }
    private fun ring(stroke: Int, radius: Int, width: Int, fill: Int = 0xFF071016.toInt()) = GradientDrawable().apply {
        setColor(fill); cornerRadius = radius.toFloat(); setStroke(dp(width), stroke)
    }
    private fun powerBackground(stroke: Int, connected: Boolean): LayerDrawable {
        fun circle(color: Int, width: Int, fill: Int = 0x00000000) = GradientDrawable().apply {
            shape = GradientDrawable.OVAL
            setColor(fill)
            setStroke(dp(width), color)
        }
        val glow = circle(if (connected) 0x3300FE7A else 0x18283945, if (connected) 7 else 4,
            TerlimoCatalogBrandTokens.BACKGROUND.toInt())
        val outer = circle(stroke, 1)
        val inner = circle(if (connected) TerlimoCatalogBrandTokens.ACCENT.toInt() else 0xFF617075.toInt(), 1)
        return LayerDrawable(arrayOf(glow, outer, inner)).apply {
            setLayerInset(1, dp(2), dp(2), dp(2), dp(2))
            setLayerInset(2, dp(5), dp(5), dp(5), dp(5))
        }
    }
    private fun dp(value: Int) = (value * resources.displayMetrics.density + .5f).toInt()
}

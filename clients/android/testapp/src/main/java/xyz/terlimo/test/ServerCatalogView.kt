package xyz.terlimo.test

import android.content.Context
import android.graphics.Color
import android.graphics.Canvas
import android.graphics.Paint
import android.graphics.Typeface
import android.graphics.drawable.GradientDrawable
import android.os.Build
import android.view.Gravity
import android.view.View
import android.widget.Button
import android.widget.LinearLayout
import android.widget.ImageView
import android.widget.TextView
import java.time.Instant

internal fun isCatalogUsable(phase: String): Boolean =
    phase in setOf("CatalogReady", "NodeAuthenticating", "ConfiguringVPN", "Connected", "SwitchingServer", "SleepPaused", "Reconnecting", "KillSwitch")

/** Native-Views catalog. Selection is always an explicit user action. */
internal class ServerCatalogView(
    context: Context,
    private val onRefresh: () -> Unit,
    private val onSelect: (String) -> Unit,
    private val onProbe: (String) -> Unit,
    private val onProbeAll: () -> Unit,
    private val onProbeAllCancel: () -> Unit,
    private val showChrome: Boolean = true,
    private val onSupport: () -> Unit = {},
) : LinearLayout(context) {
    private val density = resources.displayMetrics.density
    private var lastState: ViewState? = null
    private var lastPings: Map<String, NodePingState> = emptyMap()

    init {
        orientation = VERTICAL
        setPadding(dp(16), dp(12), dp(16), dp(24))
        setBackgroundColor(TerlimoCatalogBrandTokens.BACKGROUND.toInt())
    }

    fun render(state: ViewState, pings: Map<String, NodePingState> = lastPings) {
        lastState = state
        lastPings = pings
        removeAllViews()
        if (showChrome) {
            addAppBar(state)
            state.summary?.let(::addSubscriptionCard)
        }
        val expired = state.summary?.subscriptionExpiresAt?.let { !Instant.now().isBefore(it) } == true
        val catalogUsable = isCatalogUsable(state.phase)
        // Owner contract 2/3: the display-only browse list is the current display whenever a
        // browse answer was accepted (or an explicit offline state exists); stale retained
        // verified nodes never hide it, and its rows are never probe/admission-affecting.
        val browseMode = BrowseCatalogCodec.isDisplayed(state)
        when {
            // A fresh accepted browse answer outranks a stale retained summary/verified list:
            // the owner contract keeps the catalog visible without any admission.
            browseMode -> addBrowse(state, BrowseCatalogCodec.displayedNodes(state))
            expired -> addExpired(state)
            // Retained verified catalog after an ordinary Disconnect: show it read-only
            // (selection cannot mutate the stale cache) plus the idle/error message.
            CatalogRenderPolicy.showRetained(state.phase, state.nodes) -> {
                if (state.phase == "Error")
                    addError(state.error, retry = CatalogErrorActions.credentialRetry(state.phase, state.error))
                else addIdle()
                addContent(state, readOnly = true)
            }
            state.phase == "Idle" -> addIdle()
            state.phase == "Error" ->
                addError(state.error, retry = CatalogErrorActions.credentialRetry(state.phase, state.error))
            !catalogUsable && state.nodes.isEmpty() -> addLoading()
            !catalogUsable -> {
                addSelected(state)
                addLoading()
            }
            state.nodes.isEmpty() -> addEmpty()
            else -> addContent(state)
        }
    }

    private fun addAppBar(state: ViewState) {
        addView(row(minHeight = 64).apply {
            addView(ImageView(context).apply {
                setImageResource(R.drawable.terlimo_vpn_logo)
                layoutParams = LayoutParams(dp(48), dp(48)).apply { marginEnd = dp(10) }
                contentDescription = "Логотип TERLIMO VPN"
            })
            addView(column(weight = 1f).apply {
                addView(text("Серверы", 24f, bold = true))
                addView(text(if (state.phase == "Starting") "Обновление…" else "TERLIMO VPN", 12f, muted = true))
            })
            addView(Button(context).apply {
                text = "Обновить"
                contentDescription = "Обновить каталог серверов"
                minWidth = dp(48); minHeight = dp(48)
                isEnabled = state.phase == "CatalogReady" || state.phase == "Idle" || state.phase == "Error" ||
                    BrowseCatalogCodec.isDisplayed(state)
                setOnClickListener { onRefresh() }
            })
        })
    }

    private fun addSubscriptionCard(summary: CatalogSummary) {
        val active = summary.subscriptionExpiresAt?.let { Instant.now().isBefore(it) } ?: true
        addView(card().apply {
            addView(text("Подписка", 14f, muted = true))
            addView(text(if (active) "Активна" else "Истекла", 18f, bold = true,
                color = if (active) TerlimoCatalogBrandTokens.ACCENT.toInt() else TerlimoCatalogBrandTokens.WARNING.toInt()))
            addView(text(summary.description(), 13f, muted = true))
        }, sectionParams())
    }

    private fun addContent(state: ViewState, readOnly: Boolean = false) {
        addSelected(state)
        if (!readOnly && state.phase == "Connected" && state.error != null) {
            addError(state.error, "Не удалось переключить сервер")
        }
        addView(text(
            if (readOnly) "Последний подтверждённый список. Подключение заново проверит доступ."
            else "Переключать сервер можно вручную без ограничений",
            13f, muted = true), sectionParams())
        val projection = CatalogCardProjection.project(
            state.nodes.map { CatalogCardInput(it.id, it.name, CountryDisplay.name(it.countryCode)) },
            state.selectedNodeId,
            lastPings,
        )
        if (!readOnly && projection.selectionProblem == SelectionProblem.SAVED_NODE_REMOVED) {
            addView(card(border = TerlimoCatalogBrandTokens.ERROR.toInt()).apply {
                addView(text("ВЫБРАННЫЙ ШЛЮЗ УДАЛЁН", 12f, bold = true, color = TerlimoCatalogBrandTokens.ERROR.toInt()))
                addView(text("Сохранённый сервер больше недоступен", 16f, bold = true))
                addView(text("Выберите другой сервер ниже. Первый сервер автоматически не выбирается.", 13f, muted = true))
            }, sectionParams())
        } else if (!readOnly && projection.selectionProblem == SelectionProblem.REQUIRED) {
            addView(text("Выберите шлюз для подключения", 14f, color = TerlimoCatalogBrandTokens.WARNING.toInt()), sectionParams())
        }
        if (BatchPingControl.shouldRender(readOnly, state.phase, state.nodes.size)) {
            val anyRunning = lastPings.values.any { it is NodePingState.Running }
            val pingAll = state.pingAll
            addView(row(minHeight = 48).apply {
                if (pingAll.active) {
                    val done = (pingAll.index + 1).coerceAtMost(pingAll.total)
                    addView(text("Проверка $done/${pingAll.total}", 14f), LayoutParams(0, LayoutParams.WRAP_CONTENT, 1f))
                    addView(Button(context).apply {
                        text = "Отмена"
                        minWidth = dp(48); minHeight = dp(48)
                        setOnClickListener { onProbeAllCancel() }
                    })
                } else {
                    addView(Button(context).apply {
                        text = "Пинг всех"
                        minWidth = dp(48); minHeight = dp(48)
                        isEnabled = !anyRunning
                        setOnClickListener { onProbeAll() }
                    }, LayoutParams(0, LayoutParams.WRAP_CONTENT, 1f))
                }
            }, sectionParams())
        }
        if (state.pingAll.sorted) {
            // One flattened list after a completed common ping: measured first (ascending),
            // then honest timeout/failed/unprobeable; selection and VPN are untouched.
            CatalogSort.ranked(projection.cards).forEach { addServerRow(it, readOnly) }
        } else {
            projection.cards.groupBy { card -> state.nodes.single { it.id == card.nodeId }.countryCode }
                .forEach { (code, cards) ->
                    addView(row(minHeight = 48).apply {
                        addView(if (code.isBlank()) badge("?", TerlimoCatalogBrandTokens.BLUE.toInt()) else CountryFlagView(context, code))
                        addView(text(CountryDisplay.name(code), 16f, bold = true), LayoutParams(0, LayoutParams.WRAP_CONTENT, 1f))
                        addView(text("${cards.count { it.enabled }} доступно", 13f, muted = true))
                    })
                    cards.forEach { addServerRow(it, readOnly) }
                }
        }
    }

    private fun addSelected(state: ViewState) {
        val selected = state.nodes.singleOrNull { it.id == state.selectedNodeId } ?: return
        val ping = lastPings[selected.id]
        addView(card(border = TerlimoCatalogBrandTokens.ACCENT.toInt()).apply {
            addView(text("ВЫБРАННЫЙ ШЛЮЗ", 12f, bold = true, color = TerlimoCatalogBrandTokens.ACCENT.toInt()))
            addView(text(selected.name, 19f, bold = true))
            addView(text("${CountryDisplay.name(selected.countryCode)} · ${pingText(ping)}", 14f, muted = true))
        }, sectionParams())
    }

    private fun addServerRow(card: CatalogCard, readOnly: Boolean = false) {
        val enabled = card.enabled && !readOnly
        addView(row(minHeight = 64).apply {
            background = shape(if (card.selected) TerlimoCatalogBrandTokens.SELECTED.toInt() else TerlimoCatalogBrandTokens.SURFACE.toInt(), dp(14),
                if (card.selected) TerlimoCatalogBrandTokens.ACCENT.toInt() else TerlimoCatalogBrandTokens.DIVIDER.toInt())
            isEnabled = enabled
            isClickable = enabled
            isFocusable = enabled
            isSelected = card.selected
            contentDescription = card.contentDescription
            if (Build.VERSION.SDK_INT >= 30) stateDescription = if (card.selected) "Выбран" else "Не выбран"
            setPadding(dp(14), dp(8), dp(10), dp(8))
            addView(column(weight = 1f).apply {
                addView(text(card.name, 16f, bold = true))
                addView(text(card.stateText, 14f, color = availabilityColor(card.availability)))
            })
            addView(text(card.rttMs?.let { "$it мс" } ?: pingText(lastPings[card.nodeId]), 14f, muted = true))
            if (!readOnly) {
                addView(Button(context).apply {
                    val running = lastPings[card.nodeId] is NodePingState.Running
                    text = if (running) "Отмена" else "Ping"
                    contentDescription = if (running) "Отменить проверку ${card.name}" else "Проверить доступность ${card.name}"
                    minWidth = dp(48); minHeight = dp(48)
                    isEnabled = card.enabled && lastState?.pingAll?.active != true
                    setOnClickListener { onProbe(card.nodeId) }
                })
            }
            addView(text(if (card.selected) "●" else "○", 28f,
                color = if (card.selected) TerlimoCatalogBrandTokens.ACCENT.toInt() else TerlimoCatalogBrandTokens.MUTED_TEXT.toInt()).apply {
                importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO
                setPadding(dp(10), 0, 0, 0)
            })
            if (!readOnly) setOnClickListener { onSelect(card.nodeId) }
        }, LayoutParams(LayoutParams.MATCH_PARENT, LayoutParams.WRAP_CONTENT).apply { bottomMargin = dp(8) })
    }

    private fun addLoading() {
        addView(card().apply {
            addView(text("Обновляем каталог…", 18f, bold = true))
            repeat(3) { addView(TextView(context).apply { minHeight = dp(48); background = shape(TerlimoCatalogBrandTokens.PLACEHOLDER.toInt(), dp(12)) }, sectionParams(8)) }
        }, sectionParams())
    }

    private fun addIdle() {
        addView(card().apply {
            addView(text("Каталог ещё не открыт", 18f, bold = true))
            addView(text("Откройте сохранённую подписку или импортируйте новую.", 14f, muted = true))
            addView(Button(context).apply {
                text = "Открыть сохранённую подписку"
                minHeight = dp(48)
                isAllCaps = false
                textSize = 14f
                setTextColor(TerlimoCatalogBrandTokens.BACKGROUND.toInt())
                background = shape(TerlimoCatalogBrandTokens.ACCENT.toInt(), dp(14))
                setOnClickListener { onRefresh() }
            }, LayoutParams(LayoutParams.MATCH_PARENT, dp(48)).apply { topMargin = dp(14) })
        }, sectionParams())
    }

    private fun addError(code: String?, title: String = "Не удалось обновить каталог", retry: Boolean = false) {
        addView(card(border = TerlimoCatalogBrandTokens.ERROR.toInt()).apply {
            addView(text(title, 18f, bold = true, color = TerlimoCatalogBrandTokens.ERROR.toInt()))
            addView(text(code?.let(UserStatusText::error) ?: "Повторите обновление каталога.", 14f, muted = true))
            if (retry) {
                addView(Button(context).apply {
                    text = "Попробовать ещё раз"
                    contentDescription = "Попробовать ещё раз: повторить загрузку списка серверов"
                    minHeight = dp(48)
                    setOnClickListener { onRefresh() }
                }, LayoutParams(LayoutParams.MATCH_PARENT, dp(48)).apply { topMargin = dp(10) })
                // §29.3: the explicit support action lives on the same real error card.
                addView(Button(context).apply {
                    text = HelpContent.SUPPORT_LABEL
                    minHeight = dp(48)
                    setOnClickListener { onSupport() }
                }, LayoutParams(LayoutParams.MATCH_PARENT, dp(48)).apply { topMargin = dp(6) })
            }
        }, sectionParams())
    }

    private fun addEmpty() {
        addView(card().apply {
            gravity = Gravity.CENTER_HORIZONTAL
            addView(text("Доступных серверов пока нет", 20f, bold = true))
            addView(text("Обновите список или проверьте состояние подписки", 14f, muted = true))
            addView(Button(context).apply { text = "Обновить"; minHeight = dp(48); setOnClickListener { onRefresh() } })
        }, sectionParams())
    }

    private fun addExpired(state: ViewState) {
        val last = state.nodes.singleOrNull { it.id == state.selectedNodeId }
        addView(card().apply {
            gravity = Gravity.CENTER_HORIZONTAL
            addView(text("Подписка истекла", 22f, bold = true, color = TerlimoCatalogBrandTokens.WARNING.toInt()))
            addView(text("Выбор и подключение станут доступны после продления.", 14f, muted = true))
            if (last != null) addView(text("Последний шлюз: ${last.name}", 13f, muted = true))
        }, sectionParams())
    }

    /**
     * Owner contract 2/5: the display-only list fetched before any access. Rows are selectable
     * (host-local preference for the next connect); there is no Ping/probe, no select_node and
     * no admission state here. A successful empty list is shown as such, never as an error.
     */
    private fun addBrowse(state: ViewState, nodes: List<NodeLabel>) {
        val selected = BrowseCatalogCodec.selectedId(state)
        val selectedNode = nodes.singleOrNull { it.id == selected }
        if (selectedNode != null) {
            addView(card(border = TerlimoCatalogBrandTokens.ACCENT.toInt()).apply {
                addView(text("ВЫБРАННЫЙ ШЛЮЗ", 12f, bold = true, color = TerlimoCatalogBrandTokens.ACCENT.toInt()))
                addView(text(selectedNode.name, 19f, bold = true))
                addView(text(CountryDisplay.name(selectedNode.countryCode), 14f, muted = true))
            }, sectionParams())
        } else {
            addView(text("Выберите сервер для подключения", 14f,
                color = TerlimoCatalogBrandTokens.WARNING.toInt()), sectionParams())
        }
        val error = state.browseError ?: state.error?.takeIf {
            state.phase == "Error" || it == "ONBOARDING_INTENT_CONFLICT"
        }
        if (error != null) addError(error, if (error == "ONBOARDING_INTENT_CONFLICT")
            "Завершите текущую попытку" else "Не удалось загрузить список серверов",
            retry = CatalogErrorActions.browseRetry(error))
        if (nodes.isEmpty()) {
            if (error == null) addEmpty()
            return
        }
        addView(text("Список доступен до подключения. Выбор сохранится для следующего подключения.", 13f, muted = true),
            sectionParams())
        nodes.forEach { addBrowseRow(it, it.id == selected) }
    }

    private fun addBrowseRow(node: NodeLabel, selected: Boolean) {
        addView(row(minHeight = 64).apply {
            background = shape(if (selected) TerlimoCatalogBrandTokens.SELECTED.toInt() else TerlimoCatalogBrandTokens.SURFACE.toInt(), dp(14),
                if (selected) TerlimoCatalogBrandTokens.ACCENT.toInt() else TerlimoCatalogBrandTokens.DIVIDER.toInt())
            isClickable = true
            isFocusable = true
            isSelected = selected
            contentDescription = "Сервер ${node.name}" + if (selected) ", выбран" else ""
            if (Build.VERSION.SDK_INT >= 30) stateDescription = if (selected) "Выбран" else "Не выбран"
            setPadding(dp(14), dp(8), dp(10), dp(8))
            addView(if (node.countryCode.isBlank()) badge("?", TerlimoCatalogBrandTokens.BLUE.toInt())
                else CountryFlagView(context, node.countryCode))
            addView(column(weight = 1f).apply {
                addView(text(node.name, 16f, bold = true))
                addView(text(CountryDisplay.name(node.countryCode), 14f, muted = true))
            })
            addView(text(if (selected) "●" else "○", 28f,
                color = if (selected) TerlimoCatalogBrandTokens.ACCENT.toInt() else TerlimoCatalogBrandTokens.MUTED_TEXT.toInt()).apply {
                importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO
                setPadding(dp(10), 0, 0, 0)
            })
            setOnClickListener { onSelect(node.id) }
        }, LayoutParams(LayoutParams.MATCH_PARENT, LayoutParams.WRAP_CONTENT).apply { bottomMargin = dp(8) })
    }

    private fun row(minHeight: Int = 0) = LinearLayout(context).apply {
        orientation = HORIZONTAL; gravity = Gravity.CENTER_VERTICAL; this.minimumHeight = dp(minHeight)
    }
    private fun column(weight: Float = 0f) = LinearLayout(context).apply {
        orientation = VERTICAL
        if (weight > 0) layoutParams = LayoutParams(0, LayoutParams.WRAP_CONTENT, weight)
    }
    private fun card(border: Int? = null) = LinearLayout(context).apply {
        orientation = VERTICAL; setPadding(dp(16), dp(14), dp(16), dp(14)); minimumHeight = dp(64)
        background = shape(TerlimoCatalogBrandTokens.SURFACE.toInt(), dp(20), border)
    }
    private fun badge(label: String, color: Int) = TextView(context).apply {
        text = label; textSize = 13f; setTextColor(color); gravity = Gravity.CENTER; typeface = Typeface.DEFAULT_BOLD
        minWidth = dp(48); minHeight = dp(48); background = shape(TerlimoCatalogBrandTokens.PLACEHOLDER.toInt(), dp(14), color)
    }
    private fun text(value: String, sp: Float, bold: Boolean = false, muted: Boolean = false, color: Int? = null) = TextView(context).apply {
        text = value; textSize = sp; setTextColor(color ?: if (muted) TerlimoCatalogBrandTokens.MUTED_TEXT.toInt() else TerlimoCatalogBrandTokens.TEXT.toInt())
        if (bold) typeface = Typeface.DEFAULT_BOLD
    }
    private fun shape(fill: Int, radius: Int, stroke: Int? = null) = GradientDrawable().apply {
        setColor(fill); cornerRadius = radius.toFloat(); if (stroke != null) setStroke(dp(1), stroke)
    }
    private fun sectionParams(top: Int = 14) = LayoutParams(LayoutParams.MATCH_PARENT, LayoutParams.WRAP_CONTENT).apply { topMargin = dp(top) }
    private fun dp(value: Int) = (value * density + .5f).toInt()
    private fun pingText(ping: NodePingState?): String = when (ping) {
        is NodePingState.Success -> "${ping.rttMs} мс"
        NodePingState.Timeout -> "Тайм-аут"
        NodePingState.Failed -> "Недоступен"
        NodePingState.Busy -> "Занято (только текущий сервер)"
        is NodePingState.Running -> "Проверка…"
        else -> "Не проверен"
    }
    private fun availabilityColor(value: NodeAvailability) = when (value) {
        NodeAvailability.AVAILABLE -> TerlimoCatalogBrandTokens.ACCENT.toInt()
        NodeAvailability.TIMEOUT -> TerlimoCatalogBrandTokens.WARNING.toInt()
        NodeAvailability.UNAVAILABLE -> TerlimoCatalogBrandTokens.ERROR.toInt()
        NodeAvailability.UNKNOWN -> TerlimoCatalogBrandTokens.MUTED_TEXT.toInt()
    }
    private fun availabilityRank(card: CatalogCard) = when (card.availability) {
        NodeAvailability.AVAILABLE -> 0
        NodeAvailability.UNKNOWN -> 1
        NodeAvailability.TIMEOUT -> 2
        NodeAvailability.UNAVAILABLE -> 3
    }
}

private class CountryFlagView(context: Context, private val code: String) : View(context) {
    private val paint = Paint(Paint.ANTI_ALIAS_FLAG)
    init { minimumWidth = dp(48); minimumHeight = dp(48); importantForAccessibility = IMPORTANT_FOR_ACCESSIBILITY_NO }
    override fun onMeasure(widthMeasureSpec: Int, heightMeasureSpec: Int) = setMeasuredDimension(dp(48), dp(48))
    override fun onDraw(canvas: Canvas) {
        super.onDraw(canvas)
        val left = dp(8).toFloat(); val top = dp(15).toFloat(); val right = dp(40).toFloat(); val bottom = dp(33).toFloat()
        when (code.uppercase()) {
            "FI" -> { fill(canvas, Color.WHITE, left, top, right, bottom); fill(canvas, 0xFF2457A7.toInt(), left + dp(9), top, left + dp(14), bottom); fill(canvas, 0xFF2457A7.toInt(), left, top + dp(7), right, top + dp(12)) }
            "NL" -> { stripe(canvas, left, top, right, bottom, listOf(0xFFAE1C28.toInt(), Color.WHITE, 0xFF21468B.toInt())) }
            "DE" -> { stripe(canvas, left, top, right, bottom, listOf(Color.BLACK, 0xFFDD0000.toInt(), 0xFFFFCE00.toInt())) }
            "RU" -> { stripe(canvas, left, top, right, bottom, listOf(Color.WHITE, 0xFF0039A6.toInt(), 0xFFD52B1E.toInt())) }
            else -> { fill(canvas, TerlimoCatalogBrandTokens.PLACEHOLDER.toInt(), left, top, right, bottom); paint.color = TerlimoCatalogBrandTokens.MUTED_TEXT.toInt(); paint.textSize = dp(10).toFloat(); paint.textAlign = Paint.Align.CENTER; canvas.drawText(code, (left + right) / 2, top + dp(13), paint) }
        }
    }
    private fun stripe(canvas: Canvas, l: Float, t: Float, r: Float, b: Float, colors: List<Int>) {
        val h = (b - t) / colors.size
        colors.forEachIndexed { i, color -> fill(canvas, color, l, t + h * i, r, if (i == colors.lastIndex) b else t + h * (i + 1)) }
    }
    private fun fill(canvas: Canvas, color: Int, l: Float, t: Float, r: Float, b: Float) { paint.color = color; canvas.drawRect(l, t, r, b, paint) }
    private fun dp(value: Int) = (value * resources.displayMetrics.density + .5f).toInt()
}

internal object CountryDisplay {
    fun name(code: String): String = when (code.uppercase()) {
        "RU" -> "Россия"; "FI" -> "Финляндия"; "NL" -> "Нидерланды"; "DE" -> "Германия"
        "SE" -> "Швеция"; "EE" -> "Эстония"; "LV" -> "Латвия"; "LT" -> "Литва"
        "PL" -> "Польша"; "FR" -> "Франция"; "GB" -> "Великобритания"; "US" -> "США"
        "" -> "Страна не указана"
        else -> "Страна $code"
    }
}

package xyz.terlimo.test

import android.content.Context
import android.text.InputType
import android.view.View
import android.view.inputmethod.EditorInfo
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.TextView

/** View-only referral screen. The caller supplies an actual server account code, never an ID. */
internal class ReferralUi(
    private val context: Context,
    private val onCopy: (String) -> Unit,
    private val onShare: (String) -> Unit,
) {
    private var registered = false
    private var ownCode: String? = null

    private fun dp(value: Int): Int = (value * context.resources.displayMetrics.density).toInt()

    private fun text(value: String, size: Float = 16f): TextView = TextView(context).apply {
        text = value
        textSize = size
        setPadding(0, dp(8), 0, dp(8))
    }

    private val ownCodeStatus = text("")
    private val copy = Button(context).apply {
        text = "Скопировать мой код"
        minHeight = dp(48)
        setOnClickListener { ownCode?.let(onCopy) }
    }
    private val share = Button(context).apply {
        text = "Поделиться моим кодом"
        minHeight = dp(48)
        setOnClickListener { ownCode?.let(onShare) }
    }
    private val incoming = EditText(context).apply {
        hint = "Код пригласившего (необязательно)"
        inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
        isSingleLine = true
        imeOptions = EditorInfo.IME_ACTION_DONE or EditorInfo.IME_FLAG_NO_EXTRACT_UI or
            EditorInfo.IME_FLAG_NO_PERSONALIZED_LEARNING
        isSaveEnabled = false
        isSaveFromParentEnabled = false
        importantForAutofill = View.IMPORTANT_FOR_AUTOFILL_NO_EXCLUDE_DESCENDANTS
    }
    private val draftStatus = text("Можно зарегистрироваться без кода пригласившего.")
    private val incomingPanel = LinearLayout(context).apply {
        orientation = LinearLayout.VERTICAL
        addView(text("Вас пригласили?", 20f))
        addView(text("Укажите чужой код до регистрации в Telegram. Поле необязательное. " +
            "Отправка кода на сервер пока недоступна; введённый код не привязан к аккаунту."))
        addView(incoming)
        addView(Button(context).apply {
            text = "Проверить ввод"
            minHeight = dp(48)
            setOnClickListener {
                val raw = incoming.text.toString()
                incoming.error = null
                draftStatus.text = when {
                    raw.isBlank() -> "Код не указан. Можно зарегистрироваться без него."
                    localDraft(raw) == null -> {
                        incoming.error = "Удалите переносы строк и управляющие символы."
                        "Проверьте введённый код."
                    }
                    else -> "Код введён. Формат и привязку должен подтвердить сервер. " +
                        "Отправка пока недоступна; привязка не выполнена."
                }
            }
        })
        addView(draftStatus)
    }
    private val root = LinearLayout(context).apply {
        orientation = LinearLayout.VERTICAL
        setPadding(dp(16), dp(16), dp(16), dp(24))
        addView(text("Пригласить друга", 24f))
        addView(text("Мой код", 20f))
        addView(ownCodeStatus)
        addView(copy)
        addView(share)
        addView(text("Условия приглашения", 20f))
        addView(text("После регистрации в Telegram у аккаунта появляется постоянный уникальный " +
            "код. Им можно делиться даже без активного доступа. Приглашённый тоже получает свой код."))
        addView(text("Приглашённому: подходящий пробный период — 10 дней (7 + 3). Бесплатный " +
            "час не даёт реферального бонуса. Скидка 100 ₽ действует бессрочно на первую успешную " +
            "покупку основной подписки на 1, 3 или 6 месяцев. Пробный период для скидки не обязателен. " +
            "Отменённая или неуспешная оплата скидку не расходует."))
        addView(text("Пригласившему: +7 дней за подходящий пробный период приглашённого и отдельно " +
            "+7 / +14 / +30 дней за его первую успешную оплату основной подписки на 1 / 3 / 6 " +
            "месяцев соответственно. Без активного доступа заработанные дни ждут; сами по себе " +
            "они новый доступ не создают."))
    }

    init {
        render(telegramRegistered = false)
    }

    /** Put this content inside the tab's ScrollView. No activity or account state is owned here. */
    fun panel(): LinearLayout = root

    /** Optional invitation draft belongs to the pre-registration Home action. */
    fun invitationPanel(): LinearLayout = incomingPanel

    /** Null until the referral wire supplies this exact account's server code. */
    fun render(telegramRegistered: Boolean, serverAccountCode: String? = null) {
        registered = telegramRegistered
        ownCode = serverAccountCode?.takeIf { registered && it.isNotBlank() }
        ownCodeStatus.text = when {
            !registered -> "Ваш постоянный код станет доступен после регистрации в Telegram. " +
                "Регистрация находится во вкладке «Подписка»."
            ownCode == null -> "Получение вашего кода с сервера пока недоступно. " +
                "Копирование и отправка станут доступны после получения настоящего кода аккаунта."
            else -> "Ваш код: ${ownCode}"
        }
        copy.isEnabled = ownCode != null
        share.isEnabled = ownCode != null
        incomingPanel.visibility = if (registered) View.GONE else View.VISIBLE
    }

    /** Optional in-memory input only; this does not submit, persist or bind a referral. */
    fun incomingCodeDraft(): String? = if (registered) null else localDraft(incoming.text.toString())

    private fun localDraft(raw: String): String? =
        raw.takeIf { value -> value.none { it.isISOControl() } }?.trim()?.takeIf { it.isNotEmpty() }
}

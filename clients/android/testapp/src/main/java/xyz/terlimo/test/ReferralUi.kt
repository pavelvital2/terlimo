package xyz.terlimo.test

import android.content.Context
import android.text.Editable
import android.text.InputType
import android.text.TextWatcher
import android.view.View
import android.view.inputmethod.EditorInfo
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.TextView

/** View-only referral screen. All durable state and operations belong to the caller. */
internal class ReferralUi(
    private val context: Context,
    private val onCopy: (String) -> Unit,
    private val onShare: (String) -> Unit,
    private val onDraftChanged: (String) -> Unit = {},
    private val onSubmit: () -> Unit = {},
    private val onClear: () -> Unit = {},
    private val onRetry: () -> Unit = {},
    private val onRefresh: () -> Unit = {},
) {
    private var model = ReferralRenderModel()
    private var settingDraft = false
    private var renderedDraft: String? = null
    private var renderedDraftRevision: Long? = null
    private var ownCode: String? = null
    private var telegramLink: String? = null
    private var webLink: String? = null

    private fun dp(value: Int): Int = (value * context.resources.displayMetrics.density).toInt()

    private fun text(value: String, size: Float = 16f): TextView = TextView(context).apply {
        text = value
        textSize = size
        setPadding(0, dp(8), 0, dp(8))
    }

    private fun button(label: String, action: () -> Unit): Button = Button(context).apply {
        text = label
        minHeight = dp(48)
        setOnClickListener { action() }
    }

    private val ownCodeStatus = text("")
    private val copy = button("Скопировать мой код") { ownCode?.let(onCopy) }
    private val share = button("Поделиться моим кодом") { ownCode?.let(onShare) }
    private val telegramLinkStatus = text("")
    private val copyTelegram = button("Скопировать ссылку Telegram") { telegramLink?.let(onCopy) }
    private val shareTelegram = button("Поделиться ссылкой Telegram") { telegramLink?.let(onShare) }
    private val webLinkStatus = text("")
    private val copyWeb = button("Скопировать ссылку сайта") { webLink?.let(onCopy) }
    private val shareWeb = button("Поделиться ссылкой сайта") { webLink?.let(onShare) }
    private val rewardsStatus = text("")
    private val attributionStatus = text("")
    private val benefitsStatus = text("")
    private val referralCandidateStatus = text("")
    private val termsVersionStatus = text("", 14f)
    private val refresh = button("Обновить сведения") {
        if (model.telegramRegistered && model.infoState != ReferralInfoUiState.LOADING) onRefresh()
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
        addTextChangedListener(object : TextWatcher {
            override fun beforeTextChanged(s: CharSequence?, start: Int, count: Int, after: Int) = Unit
            override fun onTextChanged(s: CharSequence?, start: Int, before: Int, count: Int) = Unit
            override fun afterTextChanged(s: Editable?) {
                if (!settingDraft) {
                    error = null
                    onDraftChanged(s?.toString().orEmpty())
                }
            }
        })
    }
    private val draftStatus = text("")
    private val submit = button("Отправить код") {
        if (canEditDraft() && model.canSubmit) {
            val raw = incoming.text.toString()
            if (validCode(raw)) {
                incoming.error = null
                onSubmit()
            } else {
                incoming.error = "От 1 до 32 латинских букв или цифр. Введите код, а не ссылку."
            }
        }
    }
    private val clear = button("Очистить код") {
        if (canEditDraft() && model.canClear) onClear()
    }
    private val retry = button("Повторить исходный запрос") {
        if (model.canRetry) onRetry()
    }
    private val incomingPanel = LinearLayout(context).apply {
        orientation = LinearLayout.VERTICAL
        addView(text("Вас пригласили?", 20f))
        addView(text("Введите код пригласившего до регистрации в Telegram. Поле необязательное. " +
            "Сохранённый черновик отправляется только по кнопке «Отправить код». " +
            "Привязку к аккаунту подтверждает сервер после регистрации."))
        addView(incoming)
        addView(submit)
        addView(clear)
        addView(retry)
        addView(draftStatus)
    }
    private val root = LinearLayout(context).apply {
        orientation = LinearLayout.VERTICAL
        setPadding(dp(16), dp(16), dp(16), dp(24))
        addView(text("Пригласить друга", 24f))
        addView(text("Мой постоянный код", 20f))
        addView(ownCodeStatus)
        addView(copy)
        addView(share)
        addView(text("Ссылка для приглашения в Telegram", 20f))
        addView(telegramLinkStatus)
        addView(copyTelegram)
        addView(shareTelegram)
        addView(text("Ссылка для приглашения через сайт", 20f))
        addView(webLinkStatus)
        addView(copyWeb)
        addView(shareWeb)
        addView(refresh)
        addView(referralCandidateStatus)
        addView(attributionStatus)
        addView(benefitsStatus)
        addView(text("Дни за приглашения", 20f))
        addView(rewardsStatus)
        addView(text("Условия приглашения", 20f))
        addView(text("После регистрации в Telegram у аккаунта появляется постоянный уникальный " +
            "код. Им можно делиться даже без активного доступа. Приглашённый тоже получает свой код."))
        addView(text("Приглашённому: подходящий пробный период — 10 дней (7 + 3). Бесплатный " +
            "час не даёт реферального бонуса. Скидка 100 ₽ действует бессрочно на первую успешную " +
            "покупку основной подписки на 1, 3 или 6 месяцев. Пробный период для скидки не обязателен. " +
            "Отменённая или неуспешная оплата скидку не расходует. " +
            "Закрытие экрана оплаты не подтверждает отмену: завершение предыдущего счёта проверяет сервер."))
        addView(text("Пригласившему: +7 дней за подходящий пробный период приглашённого и отдельно " +
            "+7 / +14 / +30 дней за его первую успешную оплату основной подписки на 1 / 3 / 6 " +
            "месяцев соответственно, после применения оплаченной подписки. Без активного доступа " +
            "заработанные дни ждут; сами по себе они новый доступ не создают. " +
            "Продление и дополнительные устройства повторного бонуса не дают."))
        addView(termsVersionStatus)
    }

    init {
        render(model)
    }

    /** Put this content inside the tab's ScrollView. */
    fun panel(): LinearLayout = root

    /** Optional invitation input belongs to the pre-registration Home action. */
    fun invitationPanel(): LinearLayout = incomingPanel

    fun render(state: ReferralRenderModel) {
        model = state
        val editable = canEditDraft()
        // Actor updates cannot replace actively edited input. Revision explicitly replaces it.
        // Programmatic writes never call onDraftChanged.
        val replaceDraft = renderedDraft == null || renderedDraftRevision != state.draftRevision ||
            (!incoming.hasFocus() && renderedDraft != state.draft)
        if (replaceDraft && incoming.text.toString() != state.draft) {
            settingDraft = true
            try {
                incoming.setText(state.draft)
                incoming.setSelection(incoming.text.length)
                incoming.error = null
            } finally {
                settingDraft = false
            }
        }
        renderedDraft = state.draft
        renderedDraftRevision = state.draftRevision
        incoming.isEnabled = editable
        submit.isEnabled = editable && state.canSubmit
        clear.isEnabled = editable && state.canClear
        retry.text = if (state.registrationExpired) "Начать новую регистрацию" else "Повторить исходный запрос"
        retry.isEnabled = state.canRetry
        retry.visibility = if (state.canRetry || state.candidateState == ReferralCandidateUiState.UNKNOWN)
            View.VISIBLE else View.GONE
        draftStatus.text = candidateText(state)
        incomingPanel.visibility = if (!state.telegramRegistered || unresolvedCandidate(state.candidateState))
            View.VISIBLE else View.GONE
        referralCandidateStatus.text = candidateText(state)
        referralCandidateStatus.visibility = if (state.candidateState != ReferralCandidateUiState.NONE ||
            state.draft.isNotEmpty()) View.VISIBLE else View.GONE

        val ready = state.telegramRegistered && state.infoState == ReferralInfoUiState.READY
        ownCode = state.ownCode?.takeIf { ready && it.isNotBlank() }
        telegramLink = state.telegramLink?.takeIf { ready && ownCode != null && it.isNotBlank() }
        webLink = state.webLink?.takeIf { ready && ownCode != null && it.isNotBlank() }
        ownCodeStatus.text = when {
            !state.telegramRegistered -> "Ваш постоянный код станет доступен после регистрации в Telegram. " +
                "Регистрация находится во вкладке «Подписка»."
            ownCode != null -> "Ваш код: $ownCode"
            state.infoState == ReferralInfoUiState.LOADING -> "Получаем постоянный код вашего аккаунта…"
            state.infoState == ReferralInfoUiState.HISTORY_PENDING ->
                "Сервер проверяет историю рефералов. Постоянный код пока недоступен."
            else -> "Не удалось получить ваш код с сервера. Обновите сведения."
        }
        copy.isEnabled = ownCode != null
        share.isEnabled = ownCode != null
        telegramLinkStatus.text = telegramLink ?: "Ссылка Telegram пока недоступна."
        webLinkStatus.text = webLink ?: "Ссылка сайта пока недоступна."
        copyTelegram.isEnabled = telegramLink != null
        shareTelegram.isEnabled = telegramLink != null
        copyWeb.isEnabled = webLink != null
        shareWeb.isEnabled = webLink != null
        refresh.isEnabled = state.telegramRegistered && state.infoState != ReferralInfoUiState.LOADING
        rewardsStatus.text = if (ready && state.appliedDays != null && state.waitingDays != null)
            "Начислено к доступу: ${state.appliedDays} дней.\nОжидают активного доступа: ${state.waitingDays} дней."
        else "Сведения о начисленных и ожидающих днях пока недоступны."
        showOptional(attributionStatus, state.attributionMessage.takeIf { ready })
        showOptional(benefitsStatus, state.benefitsMessage.takeIf { ready })
        showOptional(termsVersionStatus, state.termsVersion?.takeIf { ready }?.let { "Версия условий: $it" })
    }

    fun incomingCodeDraft(): String? = incoming.text.toString().takeIf {
        !model.telegramRegistered && validCode(it)
    }

    private fun canEditDraft(): Boolean = model.canEdit && !model.telegramRegistered &&
        !unresolvedCandidate(model.candidateState)

    private fun unresolvedCandidate(state: ReferralCandidateUiState): Boolean = when (state) {
        ReferralCandidateUiState.SUBMITTING, ReferralCandidateUiState.UNKNOWN,
        ReferralCandidateUiState.CLEARING, ReferralCandidateUiState.LOCKED -> true
        else -> false
    }

    private fun validCode(raw: String): Boolean = raw.length in 1..32 &&
        raw.all { it in 'a'..'z' || it in 'A'..'Z' || it in '0'..'9' }

    private fun showOptional(view: TextView, value: String?) {
        view.text = value.orEmpty()
        view.visibility = if (value.isNullOrBlank()) View.GONE else View.VISIBLE
    }

    private fun candidateText(state: ReferralRenderModel): String {
        val description = state.statusMessage?.takeIf { it.isNotBlank() } ?: when (state.candidateState) {
            ReferralCandidateUiState.NONE -> "Можно зарегистрироваться без кода пригласившего."
            ReferralCandidateUiState.DRAFT -> "Черновик сохранён. Код ещё не отправлен на сервер."
            ReferralCandidateUiState.SUBMITTING -> "Отправляем код пригласившего…"
            ReferralCandidateUiState.UNKNOWN -> "Результат исходного запроса пока неизвестен. " +
                "Повтор использует тот же запрос. Код пока нельзя заменить или очистить."
            ReferralCandidateUiState.PENDING -> "Сервер принял код для следующей регистрации. " +
                "Приглашение ещё не привязано к аккаунту."
            ReferralCandidateUiState.CLEARING -> "Проверяем удаление кода на сервере…"
            ReferralCandidateUiState.LOCKED -> "Код закреплён за текущей попыткой регистрации. " +
                "Открытие Telegram не подтверждает привязку приглашения."
            ReferralCandidateUiState.CONFIRMED -> "Сервер подтвердил привязку приглашения к аккаунту."
            ReferralCandidateUiState.REJECTED -> "Сервер отклонил привязку приглашения. " +
                "Регистрация аккаунта может завершиться без него."
            ReferralCandidateUiState.HISTORY_PENDING -> "Сервер проверяет историю приглашения. " +
                "Результат пока не подтверждён."
            ReferralCandidateUiState.UNAVAILABLE -> "Сервис приглашений сейчас недоступен. " +
                "Сохранённый код не означает подтверждённую привязку."
        }
        return state.candidateCode?.takeIf { it.isNotBlank() }?.let {
            "Код исходного запроса: $it\n$description"
        } ?: description
    }
}

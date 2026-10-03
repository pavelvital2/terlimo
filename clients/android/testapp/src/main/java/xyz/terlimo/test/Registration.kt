package xyz.terlimo.test

/**
 * S3-A Telegram registration display state. It is a pure projection of the server-owned
 * `registration` block of `account_access`: the client never derives eligibility and never
 * stores Telegram identity or eligibility as truth. No trial-issuance action exists in this
 * slice; only a descriptive status is shown.
 */
internal data class RegistrationState(
    val state: String = "none",
    val withinHour: Boolean = false,
    val trialAvailable: Boolean = false,
    val trialReason: String? = null,
    val purchaseAvailable: Boolean = false,
    val error: String? = null,
)

internal object RegistrationUi {
    /** Telegram registration needs Internet, not a paid order or an unexpired hour. */
    const val INTERNET_TEXT = "Для регистрации или входа через Telegram нужен интернет. Оплата доступна после подтверждения."

    fun registerVisible(state: ViewState): Boolean {
        val projection = state.accountAccess?.projection ?: return false
        return !PurchaseFlow.usableAccount(projection)
    }

    /**
     * Explicit existing-account sign-in is offered on a fresh installation without any data
     * right (full slots included): the server decides eligibility of the existing subscription,
     * the client never guesses it and never promises a trial on this branch.
     */
    fun loginVisible(state: ViewState): Boolean {
        val projection = state.accountAccess?.projection ?: return false
        return loginVisibleFor(
            usableAccount = PurchaseFlow.usableAccount(projection),
            dataAccess = projection.grant.dataAccess,
            paidAwaitingBinding = PurchaseFlow.paidAwaitingBinding(state.purchase),
        )
    }

    fun loginVisibleFor(usableAccount: Boolean, dataAccess: String?, paidAwaitingBinding: Boolean): Boolean {
        if (usableAccount) return false
        if (paidAwaitingBinding) return false
        return dataAccess != "onboarding_hour"
    }

    fun buttonText(state: ViewState): String {
        val registration = state.registration ?: return "Зарегистрироваться в Telegram"
        return when {
            registration.error != null -> "Повторить регистрацию в Telegram"
            registration.state == "pending" -> "Ожидаем подтверждение в Telegram"
            else -> "Зарегистрироваться в Telegram"
        }
    }

    /** Descriptive status only: no active "get trial" action is created before the endpoint. */
    fun statusText(state: ViewState): String? {
        val registration = state.registration
        // A real registration error/retry stays visible above the paid call: the user must see
        // why the flow failed, not a generic instruction (S5 §3.2C).
        if (registration?.error != null) {
            return when (registration.error) {
                "REGISTRATION_DISABLED" -> "Регистрация временно недоступна."
                "REGISTRATION_ALREADY_DONE" -> "Регистрация уже подтверждена."
                "ACCESS_DENIED" -> "Регистрация недоступна для этой установки."
                else -> "Не удалось начать регистрацию. Повторите позже."
            }
        }
        // A paid order awaiting binding: the user must register in Telegram so the parked
        // payment can be applied, even if the hour expired (S5 §3.2C).
        if (PurchaseFlow.paidAwaitingBinding(state.purchase) &&
            !PurchaseFlow.usableAccount(state.accountAccess?.projection)) {
            return "Оплата получена. Зарегистрируйтесь в Telegram, чтобы применить оплаченный доступ."
        }
        if (PurchaseFlow.usableAccount(state.accountAccess?.projection)) {
            return "Вход через Telegram подтверждён."
        }
        val status = registration ?: return if (registerVisible(state)) INTERNET_TEXT else null
        return when (status.state) {
            "pending" -> "Завершите регистрацию в Telegram."
            "registered" -> "Подтверждение Telegram получено. Вход в аккаунт ещё не подтверждён сервером."
            else -> INTERNET_TEXT
        }
    }
}

/**
 * S3-B explicit trial activation display state. Server truth only (`/me.trial`): the action is
 * offered exactly when `can_activate` is true and an endpoint answered. A replay/expired result
 * shows the used state and never offers a repeat issuance. No client-side eligibility exists.
 */
internal data class TrialState(
    val state: String = "none",
    val canActivate: Boolean = false,
    val reason: String? = null,
    val startsAt: String? = null,
    val endsAt: String? = null,
    val error: String? = null,
)

internal object TrialUi {
    /** The explicit activation action is offered only on the server's can_activate signal. */
    fun activateVisible(state: ViewState): Boolean {
        val trial = state.accountAccess?.projection?.trial ?: return false
        if (!trial.canActivate) return false
        return state.trial?.error == null
    }

    fun buttonText(state: ViewState): String = "Получить 7 дней"

    /**
     * Descriptive status. A used/expired replay is shown as used and never as active, and no
     * repeat issuance is offered.
     */
    fun statusText(state: ViewState): String? {
        val error = state.trial?.error
        if (error != null) {
            return when (error) {
                "REGISTRATION_REQUIRED" -> "Сначала зарегистрируйтесь в Telegram."
                "TRIAL_NOT_ELIGIBLE" -> "Пробный доступ недоступен: час истёк до регистрации."
                "CHANNEL_MEMBERSHIP_REQUIRED" ->
                    "Нужна подписка на официальный канал TERLIMO. Подпишитесь и повторите проверку."
                "SUBSCRIPTION_ACTIVE" -> "У вас уже действует подписка."
                "TRIAL_ALREADY_USED" -> "Пробный доступ уже использован."
                "TRIAL_CHECK_UNAVAILABLE" -> "Проверка пробного доступа временно недоступна. Повторите позже."
                "TRIAL_NOT_AVAILABLE" -> "Пробный доступ сейчас недоступен для этой установки."
                "TRIAL_CONTROL_UNAVAILABLE" -> "Не удалось запустить служебное подключение для оформления пробного доступа."
                "TRIAL_CONTROL_TIMEOUT" -> "Служебное подключение не ответило. Повторите позже."
                else -> "Не удалось получить пробный доступ. Повторите позже."
            }
        }
        val trial = state.accountAccess?.projection?.trial ?: return null
        return when (trial.state) {
            "available" -> "Доступен пробный доступ на 7 дней."
            "active" -> "Пробный доступ активен."
            "used" -> "Пробный доступ уже использован."
            "ineligible" ->
                if (trial.reason == "hour_expired_before_registration" || trial.reason == "hour_expired")
                    "Пробный доступ недоступен: час истёк до регистрации."
                else "Пробный доступ недоступен."
            else -> null
        }
    }
}

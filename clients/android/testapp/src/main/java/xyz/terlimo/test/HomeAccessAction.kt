package xyz.terlimo.test

/** Display-only choice. Invoking an operation still requires an explicit click and its existing guard. */
internal enum class HomeAccessAction(val label: String) {
    NONE(""), HOUR("Активировать 1 час для регистрации"),
    REGISTER("Перейти к регистрации в Telegram"), TRIAL("Активировать пробный период"),
    PURCHASE("Выбрать подписку");

    companion object {
        fun forState(state: ViewState, pendingChoice: Boolean): HomeAccessAction {
            if (state.recoveryStatus == "RECOVERY_RUNNING") return NONE
            val projection = state.accountAccess?.projection ?: return NONE
            if (!PurchaseFlow.usableAccount(projection)) {
                return if (PreAdmissionConnect.eligible(state) && PreAdmissionConnect.connectable(state, pendingChoice))
                    HOUR else REGISTER
            }
            // Active access keeps the ordinary connect/disconnect and deadline in OrbitHomeHeader.
            if (projection.grant.dataAccess == "subscription_data") return NONE
            if (TrialUi.activateVisible(state)) return TRIAL
            return PURCHASE
        }
    }
}

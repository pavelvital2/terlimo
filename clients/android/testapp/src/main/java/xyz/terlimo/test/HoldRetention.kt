package xyz.terlimo.test

/**
 * Pure decision for the already-active KillSwitch hold branch (S5 §07.1 freshness).
 *
 * While a hold is already active the terminal handler must still never leave a `/me`
 * snapshot marked current: if a current snapshot is present it is invalidated even when
 * the hold reason is unchanged (an accepted account_access can have landed after the
 * hold started). The returned state is null when neither the error nor the freshness
 * flag would change, so the existing hold stop-wait/armed-choice semantics are preserved
 * and no spurious publish happens. Display-state only; it never changes hold behaviour.
 */
internal object HoldRetention {
    fun alreadyHeld(state: ViewState, code: String): ViewState? {
        val currentAccess = state.accountAccess?.takeIf { it.current }
        return when {
            currentAccess != null -> state.copy(error = code, accountAccess = currentAccess.copy(current = false))
            state.error != code -> state.copy(error = code)
            else -> null
        }
    }
}

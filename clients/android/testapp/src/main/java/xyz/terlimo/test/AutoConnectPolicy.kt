package xyz.terlimo.test

import android.content.Context

/**
 * §26.2 auto-connect preference and launch token (per user). Off by default and independent
 * from the §26.5 catalog schedule: turning the schedule off never cancels a user connect.
 *
 * The token is a monotonic generation kept in the same private prefs. A genuine user launch
 * arms a new generation and passes it to the Service; Off / Disconnect / consent denial /
 * import invalidate the current generation, so any in-flight or queued native intent of an
 * older generation is dropped by the Service instead of reviving a consumed request.
 */
internal object AutoConnectPrefs {
    const val PREFS = "terlimo-autoconnect"
    const val KEY_ENABLED = "enabled"
    const val KEY_GENERATION = "generation"
    const val EXTRA_GENERATION = "autoconnect_generation"

    fun isEnabled(context: Context): Boolean =
        runCatching { context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getBoolean(KEY_ENABLED, false) }
            .getOrDefault(false)

    fun setEnabled(context: Context, enabled: Boolean) {
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putBoolean(KEY_ENABLED, enabled).apply()
    }

    fun generation(context: Context): Long =
        runCatching { context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getLong(KEY_GENERATION, 0L) }
            .getOrDefault(0L)

    /** A new user launch token (cold start or a real launcher re-open). */
    fun armLaunch(context: Context): Long = bump(context)

    /** Invalidate any in-flight token (Off / Disconnect / consent denial / import/deeplink). */
    fun invalidate(context: Context): Long = bump(context)

    private fun bump(context: Context): Long {
        val next = generation(context) + 1
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putLong(KEY_GENERATION, next).apply()
        return next
    }
}

/** Identity fence: a genuine account change, never the initial null→account attach. */
internal object AutoConnectAccountFence {
    fun isIdentitySwitch(previousAccountRef: String?, updatedAccountRef: String?): Boolean =
        previousAccountRef != null && updatedAccountRef != null && previousAccountRef != updatedAccountRef
}

/**
 * Facts the auto-connect controller needs for one evaluation. [generation] is the token the
 * caller is acting on and is fenced by the controller; [lastNodeId] is already account-scoped
 * by the caller (a mismatch reads as null, never as another identity's server).
 */
internal data class AutoConnectRuntime(
    val enabled: Boolean,
    val generation: Long,
    val accountKnown: Boolean,
    val accountRef: String?,
    val lastNodeId: String?,
    val nodePresent: Boolean,
    val entitlementUsable: Boolean,
    val dataConnected: Boolean,
)

internal sealed class AutoConnectEffect {
    object None : AutoConnectEffect()
    /** Ensure one bounded attempt exists (cold start has no catalog yet). */
    object StartAttempt : AutoConnectEffect()
    /** Visible, honest status through the existing error channel (a stable code). */
    data class Message(val code: String) : AutoConnectEffect()
    /** Carries the planning-time token/target/account so execution can revalidate them. */
    data class Choose(val generation: Long, val target: String, val accountRef: String?) : AutoConnectEffect()
    data class Select(val generation: Long, val target: String, val accountRef: String?) : AutoConnectEffect()
    object Consume : AutoConnectEffect()
}

/**
 * Pure §26.2 sequencer. Owns at most one target per launch generation: at most one choose,
 * then the existing explicit select. A connected-elsewhere outcome, a missing/removed server
 * or a lost right is a message + consumed token, never a silent fallback to another node.
 *
 * Every effect carries the generation/account it was planned under; the adapter revalidates
 * them immediately before any native command. A consumed generation can never be re-armed.
 */
internal class AutoConnectController {
    internal enum class Stage { IDLE, WAIT_ACCOUNT, WAIT_CATALOG, CHOOSE_SENT, SELECT_READY, SELECT_SENT, DONE }

    var stage: Stage = Stage.IDLE
        private set
    var target: String? = null
        private set
    private var token: Long = 0L
    private var consumed = true
    private var lastConsumed: Long = 0L

    fun isArmed(): Boolean = !consumed && token != 0L

    /** The armed token must still match the generation the caller is acting on. */
    fun isArmedFor(generation: Long): Boolean = isArmed() && token == generation

    private var plannedAccount: String? = null

    /**
     * User launch, or a fresh account projection delivered while waiting for it. A consumed
     * generation is terminal: the same token is never re-armed, only a newer one starts a run.
     */
    fun onLaunch(rt: AutoConnectRuntime): List<AutoConnectEffect> {
        if (!rt.enabled) return listOf(AutoConnectEffect.None)
        if (consumed && rt.generation <= lastConsumed) return listOf(AutoConnectEffect.None)
        // A stale (older) token can never replace or corrupt the armed generation.
        if (isArmed() && rt.generation < token) return listOf(AutoConnectEffect.None)
        val duplicate = isArmed() && token == rt.generation &&
            stage != Stage.IDLE && stage != Stage.WAIT_ACCOUNT
        if (duplicate) return listOf(AutoConnectEffect.None)
        if (rt.dataConnected) { arm(rt.generation); consume(); return listOf(AutoConnectEffect.Consume) }
        if (token != rt.generation || consumed) arm(rt.generation)
        if (rt.accountKnown) return evaluate(rt)
        // Cold start: an absent /me is not an absent stored server; wait for the projection.
        stage = Stage.WAIT_ACCOUNT
        return listOf(AutoConnectEffect.StartAttempt)
    }

    /** Fresh account projection (including the first /me after a cold start). */
    fun onAccount(rt: AutoConnectRuntime): List<AutoConnectEffect> =
        if (isArmedFor(rt.generation) && stage == Stage.WAIT_ACCOUNT) evaluate(rt)
        else listOf(AutoConnectEffect.None)

    private fun evaluate(rt: AutoConnectRuntime): List<AutoConnectEffect> {
        if (!rt.enabled) { consume(); return listOf(AutoConnectEffect.Consume) }
        val last = rt.lastNodeId?.takeIf { it.isNotBlank() }
            ?: run { consume(); return listOf(AutoConnectEffect.Message(AutoConnectCode.NO_LAST), AutoConnectEffect.Consume) }
        if (!rt.entitlementUsable) {
            consume(); return listOf(AutoConnectEffect.Message(AutoConnectCode.RIGHTS), AutoConnectEffect.Consume)
        }
        target = last
        plannedAccount = rt.accountRef
        stage = Stage.WAIT_CATALOG
        return listOf(AutoConnectEffect.StartAttempt)
    }

    /**
     * A valid credential catalog of the same attempt. [selectedNodeId] is the server-persisted
     * selection; [pendingChoice] is the host's single outstanding choice. Duplicate catalogs
     * are a Wait, never a second choose/select.
     */
    fun onCatalog(rt: AutoConnectRuntime, selectedNodeId: String, pendingChoice: Boolean): List<AutoConnectEffect> {
        if (!rt.enabled) { if (isArmedFor(rt.generation)) { consume() }; return listOf(AutoConnectEffect.None) }
        if (!isArmedFor(rt.generation) || rt.dataConnected) return listOf(AutoConnectEffect.None)
        val want = target ?: return listOf(AutoConnectEffect.None)
        if (plannedAccount != rt.accountRef) {
            consume(); return listOf(AutoConnectEffect.Message(AutoConnectCode.ACCOUNT), AutoConnectEffect.Consume)
        }
        if (!rt.entitlementUsable) {
            consume(); return listOf(AutoConnectEffect.Message(AutoConnectCode.RIGHTS), AutoConnectEffect.Consume)
        }
        if (!rt.nodePresent) {
            consume(); return listOf(AutoConnectEffect.Message(AutoConnectCode.REMOVED), AutoConnectEffect.Consume)
        }
        return when (stage) {
            Stage.WAIT_CATALOG ->
                if (selectedNodeId == want && !pendingChoice) { stage = Stage.SELECT_READY; listOf(AutoConnectEffect.None) }
                else if (!pendingChoice) {
                    stage = Stage.CHOOSE_SENT
                    listOf(AutoConnectEffect.Choose(rt.generation, want, rt.accountRef))
                } else listOf(AutoConnectEffect.None)
            Stage.CHOOSE_SENT -> {
                if (selectedNodeId == want && !pendingChoice) stage = Stage.SELECT_READY
                listOf(AutoConnectEffect.None)
            }
            Stage.SELECT_READY, Stage.SELECT_SENT, Stage.DONE, Stage.IDLE, Stage.WAIT_ACCOUNT ->
                listOf(AutoConnectEffect.None)
        }
    }

    /** Once the selection is confirmed, consent decides select vs an honest message. */
    fun onSelectOpportunity(rt: AutoConnectRuntime, consentGranted: Boolean): List<AutoConnectEffect> {
        if (!rt.enabled) { if (isArmedFor(rt.generation)) { consume() }; return listOf(AutoConnectEffect.None) }
        if (!isArmedFor(rt.generation) || stage != Stage.SELECT_READY) return listOf(AutoConnectEffect.None)
        if (plannedAccount != rt.accountRef) {
            consume(); return listOf(AutoConnectEffect.Message(AutoConnectCode.ACCOUNT), AutoConnectEffect.Consume)
        }
        if (!rt.entitlementUsable) {
            consume(); return listOf(AutoConnectEffect.Message(AutoConnectCode.RIGHTS), AutoConnectEffect.Consume)
        }
        if (!consentGranted) {
            consume(); return listOf(AutoConnectEffect.Message(AutoConnectCode.CONSENT), AutoConnectEffect.Consume)
        }
        val want = target ?: return listOf(AutoConnectEffect.None)
        stage = Stage.SELECT_SENT
        return listOf(AutoConnectEffect.Select(rt.generation, want, rt.accountRef))
    }

    /** Confirmed Connected; any connection records last at the caller. */
    fun onConnected(nodeId: String): Boolean {
        if (isArmed() && stage == Stage.SELECT_SENT && nodeId == target) { consume(); return true }
        return false
    }

    /** Choose acknowledged but no select could run inside the product budget. */
    fun onAckTimeout(): List<AutoConnectEffect> {
        if (!isArmed()) return listOf(AutoConnectEffect.None)
        consume(); return listOf(AutoConnectEffect.Message(AutoConnectCode.TIMEOUT), AutoConnectEffect.Consume)
    }

    /** Off / Disconnect / cancel / import / teardown. */
    fun disarm() { consume(); token = 0L }

    private fun arm(generation: Long) { token = generation; consumed = false; target = null; plannedAccount = null; stage = Stage.IDLE }
    private fun consume() { consumed = true; lastConsumed = maxOf(lastConsumed, token); target = null; plannedAccount = null; stage = Stage.DONE }
}

/** Stable error codes rendered by [UserStatusText]; never arbitrary Russian text. */
internal object AutoConnectCode {
    const val ACCOUNT = "AUTOCONNECT_ACCOUNT_CHANGED"
    const val NO_LAST = "AUTOCONNECT_NO_LAST"
    const val REMOVED = "AUTOCONNECT_REMOVED"
    const val RIGHTS = "AUTOCONNECT_RIGHTS"
    const val CONSENT = "AUTOCONNECT_CONSENT"
    const val TIMEOUT = "AUTOCONNECT_TIMEOUT"
}

/** §26.2: last server is recorded on ANY confirmed connection, independent of auto target. */
internal object AutoConnectLastWrite {
    fun nodeFor(complete: Boolean, nodeId: String): String? =
        if (complete && nodeId.isNotBlank()) nodeId else null
}

/**
 * Activity-side launch bookkeeping: a single token covers arm → (optional consent) → send,
 * so a dialog and a no-dialog send behave identically and recreation restores the same state.
 * Import/deeplink/cancel invalidate the token; a late consent callback of an invalid token
 * can never send anything.
 */
internal class AutoConnectLaunchToken {
    private var generation: Long = 0L
    private var consentPending = false

    fun started(): Boolean = generation != 0L
    fun currentGeneration(): Long = generation

    /** Arms a fresh token. Returns the generation to use, or 0 when one is already pending. */
    fun start(newGeneration: Long): Long {
        if (generation != 0L) return 0L
        generation = newGeneration
        consentPending = false
        return newGeneration
    }

    fun needsConsent() {
        if (generation != 0L) consentPending = true
    }

    fun sent() {
        generation = 0L
        consentPending = false
    }

    /** Consent returned for [returnedGeneration]: only the same, still-pending token may send. */
    fun consentReturned(returnedGeneration: Long, stillValid: Boolean): Boolean {
        val allowed = consentPending && returnedGeneration == generation && stillValid
        generation = 0L
        consentPending = false
        return allowed
    }

    fun invalidate() {
        generation = 0L
        consentPending = false
    }

    /** Recreation: the pending consent token survives exactly, a sent/absent one stays absent. */
    fun restore(generation: Long, pending: Boolean) {
        this.generation = generation
        this.consentPending = pending
    }
}

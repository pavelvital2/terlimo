package xyz.terlimo.test

import java.time.Duration
import java.time.Instant

/**
 * Bounded session-generation chain plus the frozen UI clock for the account_access
 * projection (contract sections 3.1, 5 and 8.1).
 *
 * The host keeps only the pair `current_gen` + `stored_prev`; it never keeps a
 * retired-generation set and relies on the native lifetime no-reuse invariant.
 * Session generations are equality correlation only: never compared numerically and
 * never compared to revisions.
 */
internal data class AccountAccessChain(val currentGen: String? = null, val storedPrev: String? = null)

internal data class AccountAccessSnapshot(
    val projection: AccountAccessProjection,
    val receivedElapsed: Long,
    val chain: AccountAccessChain,
    // True only while this snapshot is the current successful `/me` of the live attempt.
    // A terminal stop/hold keeps the projection (accepted last-good for status/term) but
    // marks it not current so account-usage-style lines never present it as live.
    val current: Boolean = true,
)

internal object AccountAccessPolicy {
    /**
     * Returns the updated pair, or null when the event must be ignored. Rules:
     * first event must be unlinked (previous == null); same-pair repeats are accepted;
     * a replacement must link `previous == current` and change the generation.
     */
    fun accept(chain: AccountAccessChain, generation: String, previous: String?): AccountAccessChain? {
        if (generation == chain.currentGen) {
            return if (previous == chain.storedPrev) chain else null
        }
        if (chain.currentGen == null) {
            return if (previous == null) AccountAccessChain(generation, null) else null
        }
        return if (previous == chain.currentGen) AccountAccessChain(generation, chain.currentGen) else null
    }

    /**
     * Applies one parsed event. A rejected chain keeps the last good snapshot; the
     * projection is never merged partially.
     */
    fun apply(
        current: AccountAccessSnapshot?,
        incoming: AccountAccessProjection,
        receivedElapsed: Long,
    ): AccountAccessSnapshot? {
        val chain = accept(current?.chain ?: AccountAccessChain(),
            incoming.sessionGeneration, incoming.previousSessionGeneration) ?: return null
        return AccountAccessSnapshot(incoming, receivedElapsed, chain, current = true)
    }

    /**
     * Display-only status line for the existing subscription text. It never gates access:
     * zero remaining is a screen estimate, not local revocation. Precedence for the
     * onboarding hour: an existing prohibition that already stops the data plane natively
     * (revoked session, revoked/deactivated binding, revoked/expired/unreviewed
     * entitlement) shows that precise factual reason; otherwise a confirmed active hour is
     * shown as the hour itself, whatever the eligible business account state says (the two
     * are distinct); every other accepted right keeps the state prefix. Catalog/grant
     * network health is never inferred from the account snapshot here.
     */
    fun statusLine(snapshot: AccountAccessSnapshot, nowElapsed: Long): String {
        if (snapshot.projection.grant.dataAccess == ONBOARDING_HOUR) {
            onboardingHourBlockedReason(snapshot)?.let { return it }
            return if (isConfirmedOnboardingHour(snapshot, nowElapsed))
                onboardingHourText(remainingMillis(snapshot, nowElapsed), snapshot.projection.account.telegramLinked)
            else ONBOARDING_HOUR_END_TEXT
        }
        val state = when (snapshot.projection.account.state) {
            "UNLINKED" -> "Аккаунт не привязан"
            "VERIFIED_NO_ENTITLEMENT" -> "Подписка не активна"
            "VERIFIED_NO_SLOT" -> "Нет свободного места"
            "ACTIVE_TRIAL", "ACTIVE_PAID" -> "Доступ активен"
            "EXPIRED" -> ENTITLEMENT_EXPIRED_TEXT
            "REVOKED_SESSION" -> "Сессия отозвана"
            else -> "Статус доступа неизвестен"
        }
        return "$state · ${accessText(snapshot, nowElapsed)}"
    }

    /**
     * The single display predicate of the hour countdown, shared by the main status, the
     * subscription status, the notification line and the tick (re)arm. True only for the
     * contract-permitted combination the display may count down: the effective grant
     * carries `onboarding_hour`, the onboarding block is `active`, the frozen server
     * deadline still has time left and no existing snapshot prohibition blocks it.
     * EXPIRED account state is deliberately allowed, so genuine account expiry cannot hide
     * the confirmed hour; paid/trial and every other right are unchanged. This never
     * validates, admits, stops or extends anything: native stays the stop owner.
     */
    fun isConfirmedOnboardingHour(snapshot: AccountAccessSnapshot, nowElapsed: Long): Boolean =
        snapshot.projection.grant.dataAccess == ONBOARDING_HOUR &&
            snapshot.projection.onboarding.state == "active" &&
            (remainingMillis(snapshot, nowElapsed) ?: 0L) > 0L &&
            onboardingHourBlockedReason(snapshot) == null

    /**
     * Display-only block reason of the hour, or null when no existing prohibition applies.
     * It mirrors the existing native stop vocabulary on the same snapshot fields and
     * intentionally does not repeat the native admission: session, binding and entitlement
     * only; catalog validity and transport admission health are never inferred from the
     * snapshot. The reason outranks both the hour and the owner end text, so a revoked or
     * expired right never renders as a merely absent or finished hour.
     */
    fun onboardingHourBlockedReason(snapshot: AccountAccessSnapshot): String? = when {
        snapshot.projection.account.state == "REVOKED_SESSION" -> REVOKED_SESSION_REASON_TEXT
        snapshot.projection.account.bindingStatus in BLOCKING_BINDING_STATUSES -> BINDING_REASON_TEXT
        snapshot.projection.entitlement.status == "revoked" -> ENTITLEMENT_REVOKED_TEXT
        snapshot.projection.entitlement.status == "expired" -> ENTITLEMENT_EXPIRED_TEXT
        snapshot.projection.entitlement.status == "unknown_review" -> ENTITLEMENT_UNREVIEWED_TEXT
        else -> null
    }

    private fun accessText(snapshot: AccountAccessSnapshot, nowElapsed: Long): String {
        val remaining = remainingMillis(snapshot, nowElapsed)
        return when (snapshot.projection.grant.dataAccess) {
            "subscription_data" -> if (remaining == null) "Подписка активна" else "Подписка: осталось ${formatRemaining(remaining)}"
            "restricted_checkout" -> "VPN не активен; доступна оплата"
            else -> "VPN не активен"
        }
    }

    /**
     * The onboarding hour is titled by its purpose, never as a trial. The countdown is the
     * server deadline through [remainingMillis]; ending it uses the owner wording (activate
     * trial or pay, checkout stays available) and promises nothing about Telegram. The
     * ten-minute warning names the action per linked state; it is display-only and never
     * gates, stops or extends access.
     */
    private fun onboardingHourText(remaining: Long?, telegramLinked: Boolean): String = when {
        remaining == null -> "Доступ для регистрации активен"
        remaining == 0L -> ONBOARDING_HOUR_END_TEXT
        remaining <= ONBOARDING_WARNING_MILLIS ->
            "Доступ для регистрации: осталось ${formatRemaining(remaining)} · скоро завершится" +
                if (telegramLinked) " · Оформите доступ" else " · Зарегистрируйтесь и оформите доступ"
        else -> "Доступ для регистрации: осталось ${formatRemaining(remaining)}"
    }

    /**
     * Short display-only line for the foreground VPN notification. Null for every other
     * right, so paid/trial/checkout/none can never carry onboarding wording, and also null
     * when an existing prohibition blocks the hour: a revoked/expired/unreviewed right
     * never gets the hour, the owner end text or a CTA in the notification. A confirmed
     * active hour is shown regardless of the eligible business account state; once the
     * hour ends it carries the owner end text, not a promise.
     */
    fun notificationLine(snapshot: AccountAccessSnapshot?, nowElapsed: Long): String? {
        if (snapshot == null || snapshot.projection.grant.dataAccess != ONBOARDING_HOUR) return null
        if (onboardingHourBlockedReason(snapshot) != null) return null
        return if (isConfirmedOnboardingHour(snapshot, nowElapsed))
            onboardingHourText(remainingMillis(snapshot, nowElapsed), snapshot.projection.account.telegramLinked)
        else ONBOARDING_HOUR_END_TEXT
    }

    private const val ONBOARDING_HOUR = "onboarding_hour"
    private const val ONBOARDING_WARNING_MILLIS = 600_000L
    private val BLOCKING_BINDING_STATUSES = setOf("revoked", "deactivated")

    /** Precise display reasons for the existing prohibitions; factual, short, no promise. */
    internal const val REVOKED_SESSION_REASON_TEXT = "Сессия отозвана · VPN не активен"
    internal const val BINDING_REASON_TEXT = "Доступ устройства отозван · VPN не активен"
    internal const val ENTITLEMENT_REVOKED_TEXT = "Подписка отозвана"
    internal const val ENTITLEMENT_EXPIRED_TEXT = "Срок доступа истёк"
    internal const val ENTITLEMENT_UNREVIEWED_TEXT = "Подписка на проверке"

    /** Owner wording for the finished hour: trial/pay action, checkout available, no Telegram promise. */
    internal const val ONBOARDING_HOUR_END_TEXT =
        "Час доступа завершён. Для подключения активируйте пробный период или оплатите подписку. Оплата остаётся доступна."

    private fun formatRemaining(millis: Long): String {
        val totalSeconds = millis / 1000
        val hours = totalSeconds / 3600
        val minutes = (totalSeconds % 3600) / 60
        val seconds = totalSeconds % 60
        return if (hours > 0) String.format("%d:%02d:%02d", hours, minutes, seconds)
        else String.format("%02d:%02d", minutes, seconds)
    }

    /**
     * Frozen UI clock: UTC instants, anchored at receipt with the host monotonic clock.
     * Returns null when the grant carries no deadline (no countdown). Zero is display
     * only and is never local revocation or admission.
     */
    fun remainingMillis(snapshot: AccountAccessSnapshot, nowElapsed: Long): Long? {
        val deadline = snapshot.projection.grant.effectiveDeadline ?: return null
        val remainingAtReceive = Duration.between(
            Instant.parse(snapshot.projection.serverTime), Instant.parse(deadline),
        ).toMillis().coerceAtLeast(0)
        val elapsedSinceReceive = (nowElapsed - snapshot.receivedElapsed).coerceAtLeast(0)
        return (remainingAtReceive - elapsedSinceReceive).coerceAtLeast(0)
    }
}

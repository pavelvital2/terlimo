package xyz.terlimo.test

import java.time.Instant
import java.time.ZoneId

/**
 * Display-only term line for the accepted /me entitlement (S5). It renders the active
 * trial/paid expiry in the device-local timezone and never claims a date when the field
 * is absent: a missing valid_until/trial end becomes an explicit "срок не указан" state,
 * a perpetual commercial right is shown as indefinite, and an expired/revoked/unreviewed
 * right is shown as such. A paid purchase that the fresh /me has not yet reflected stays
 * pending here too. No value is fabricated and nothing here writes the projection.
 */
internal object SubscriptionTermText {
    const val AWAITING_CONFIRMATION_TEXT = "Оплата получена. Ожидаем подтверждение подписки по серверу…"

    fun term(
        projection: AccountAccessProjection,
        purchase: PurchaseState?,
        zone: ZoneId,
    ): String? {
        if (purchase?.phase == PurchaseFlow.AWAITING_CONFIRMATION) {
            // Truthful pending state: the payment is paid but the fresh /me does not show
            // the right yet. The term is not claimed from the payment status alone.
            return AWAITING_CONFIRMATION_TEXT
        }
        if (projection.account.state == "EXPIRED") return expiredText(projection, zone)
        return when (projection.entitlement.status) {
            "active" -> activeText(projection, zone)
            "expired" -> expiredText(projection, zone)
            "revoked" -> "Подписка отозвана."
            "unknown_review" -> "Подписка на проверке."
            else -> null
        }
    }

    private fun activeText(projection: AccountAccessProjection, zone: ZoneId): String {
        val entitlement = projection.entitlement
        if (entitlement.type == "trial") {
            val endsAt = projection.trial.endsAt ?: entitlement.validUntil
                ?: return "Пробный доступ активен, срок сервером не указан."
            val instant = LocalStamp.parseUtc(endsAt) ?: return "Пробный доступ активен, срок сервером не указан."
            return "Пробный доступ действует до ${LocalStamp.format(instant, zone)} (местное время)."
        }
        val validUntil = entitlement.validUntil
        return when {
            validUntil != null -> {
                val instant = LocalStamp.parseUtc(validUntil)
                    ?: return "Подписка активна, срок сервером не указан."
                "Подписка действует до ${LocalStamp.format(instant, zone)} (местное время)."
            }
            entitlement.perpetualCommercial -> "Подписка действует без ограничения срока."
            else -> "Подписка активна, срок сервером не указан."
        }
    }

    private fun expiredText(projection: AccountAccessProjection, zone: ZoneId): String {
        val instant = projection.entitlement.validUntil?.let(LocalStamp::parseUtc)
            ?: return "Срок подписки истёк."
        return "Срок подписки истёк: ${LocalStamp.format(instant, zone)} (местное время)."
    }
}

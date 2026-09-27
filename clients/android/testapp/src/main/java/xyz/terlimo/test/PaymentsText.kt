package xyz.terlimo.test

import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter

/** Shared local-time rendering for accepted server UTC instants; never a fabricated date. */
internal object LocalStamp {
    private val FORMAT = DateTimeFormatter.ofPattern("dd.MM.yyyy HH:mm")

    /** Understandable local date/time in the device timezone (or the injected zone in tests). */
    fun format(instant: Instant, zone: ZoneId): String = FORMAT.format(instant.atZone(zone))

    /** UTC instant parse of a contract UtcTime; null when the value cannot be read. */
    fun parseUtc(value: String): Instant? = runCatching { Instant.parse(value) }.getOrNull()
}

/**
 * Display-only rendering of the frozen payment contract fields. The contract carries no QR
 * data and no checkout capability flag, so this layer never fabricates a QR and never offers
 * a hosted checkout: [hostedCheckoutOffered] is always false for the frozen vocabulary, and
 * only the provider-issued `checkout_reference` is shown, verbatim, when present.
 */
internal object PaymentsText {
    /** Exact duration labels required by the accepted S5 UI contract. */
    fun durationLabel(durationCode: String): String? = when (durationCode) {
        "days:30" -> "30 дней"
        "months:3" -> "3 месяца"
        "months:6" -> "6 месяцев"
        else -> null
    }

    fun methodLabel(method: String): String? = when (method) {
        "card" -> "Карта"
        "sbp" -> "СБП"
        "crypto" -> "Криптовалюта"
        else -> null
    }

    /**
     * Price from the exact integer minor amount plus the ISO currency code. The frozen
     * payment contract gives no currency exponent, so the display uses the two-decimal
     * convention of the frozen currency set; no conversion and no rounding of server data.
     */
    fun priceLabel(amountMinor: Long, currency: String): String {
        require(amountMinor >= 0) { "PAYMENTS_INVALID" }
        val major = groupDigits(amountMinor / 100)
        val fraction = (amountMinor % 100).toString().padStart(2, '0')
        return "$major,$fraction $currency"
    }

    /** One selectable plan row: exact server title, exact duration label, price and methods. */
    fun planLine(plan: PaymentPlan): String {
        val duration = durationLabel(plan.durationCode) ?: plan.durationCode
        val name = if (plan.title.isBlank() || plan.title == duration) duration else "${plan.title} ($duration)"
        val methods = plan.methods.mapNotNull(::methodLabel).joinToString(", ")
        val price = priceLabel(plan.amountMinor, plan.currency)
        return if (methods.isEmpty()) "$name · $price" else "$name · $price · $methods"
    }

    fun quoteLine(quote: PaymentQuote, zone: ZoneId): String {
        val duration = durationLabel(quote.durationCode) ?: quote.durationCode
        val method = methodLabel(quote.method) ?: quote.method
        val expiry = LocalStamp.parseUtc(quote.expiresAt)
            ?.let { " · действует до ${LocalStamp.format(it, zone)} (местное время)" } ?: ""
        return "$duration · ${priceLabel(quote.amountMinor, quote.currency)} · $method$expiry"
    }

    fun paymentStatusText(payment: PaymentStatusView): String = when (payment.paymentStatus) {
        "created" -> "Платёж создан. Ожидаем оплату."
        "pending" -> "Ожидаем оплату."
        "paid" -> "Оплата получена. Подтверждаем подписку по серверу…"
        "failed" -> "Платёж не прошёл."
        "expired" -> "Срок оплаты истёк."
        "refunded" -> "Платёж возвращён."
        "disputed" -> "Платёж оспаривается."
        else -> "Статус оплаты неизвестен."
    }

    fun applicationStateText(state: String): String? = when (state) {
        "not_requested" -> null
        "pending" -> "Заявка на доступ обрабатывается сервером."
        "applied" -> "Заявка на доступ обработана сервером."
        "retryable_failure" -> "Сервер временно не смог применить доступ и повторит попытку."
        "rejected" -> "Сервер отклонил применение доступа."
        else -> null
    }

    /** Provider-issued reference, shown verbatim only when present; never invented. */
    fun checkoutReferenceText(payment: PaymentStatusView): String? =
        payment.checkoutReference?.takeIf { it.isNotBlank() }?.let { "Код оплаты: $it" }

    /**
     * The frozen bridge vocabulary (cdbef94) exposes no checkout capability field and no
     * checkout-session host action, so a hosted checkout option can never be honestly
     * offered in this slice. It may only be surfaced if the contract later carries an
     * explicit capability flag.
     */
    const val hostedCheckoutOffered = false

    /**
     * §3.2B: the user tap opens the provider-issued HTTPS checkout URL in the system
     * browser. Final button labels/set follow the working-bot matrix from root; the
     * browser launch and its honest errors are independent of those texts.
     */
    const val browserCheckoutEnabled = true

    /** Explicit continue/retry of an already-created payment; it never creates a new order. */
    const val CONTINUE_PAYMENT_TEXT = "Продолжить оплату"

    const val CHECKOUT_NO_BROWSER_TEXT =
        "Не удалось открыть браузер для оплаты. Заказ сохранён — проверьте статус оплаты."
    const val CHECKOUT_INVALID_LINK_TEXT =
        "Сервис не вернул ссылку на оплату. Заказ сохранён — проверьте статус оплаты."

    /** Single-flight: one explicit purchase request is outstanding; no action may be repeated. */
    const val PURCHASE_SENDING_TEXT = "Отправляем запрос. Дождитесь ответа сервера."

    const val UNAVAILABLE_TEXT = "Покупка временно недоступна. Попробуйте позже."
    const val CHECK_AVAILABILITY_TEXT = "Проверьте тарифы и доступность оплаты на сервере."
    const val PROVIDER_UNAVAILABLE_TEXT = "Платёжный провайдер недоступен. Оплата сейчас невозможна."

    fun isUnavailable(code: String?): Boolean = code == "PROVIDER_UNAVAILABLE" || code == "SERVICE_UNAVAILABLE"

    fun errorText(code: String?): String = when (code) {
        "PROVIDER_UNAVAILABLE" -> PROVIDER_UNAVAILABLE_TEXT
        "SERVICE_UNAVAILABLE" -> UNAVAILABLE_TEXT
        "INVALID_REQUEST" -> "Запрос покупки отклонён. Проверьте выбор тарифа."
        "MOBILE_STATE_UNAVAILABLE" -> "Сервис покупки сейчас недоступен. Повторите позже."
        "BUSY" -> "Сервис покупки занят. Повторите позже."
        "TRANSPORT" -> "Нет связи с сервисом покупки. Повторите позже."
        "QUOTE_EXPIRED" -> "Предложение истекло. Оформите его заново."
        "METHOD_UNAVAILABLE" -> "Этот способ оплаты сейчас недоступен."
        "PAYMENT_NOT_FOUND" -> "Платёж не найден. Проверьте статус позже."
        "PAYMENT_STATE_INVALID" -> "Сервер сообщил противоречивый статус платежа."
        "IDEMPOTENCY_CONFLICT" -> "Повторный запрос с изменёнными данными отклонён. Начните покупку заново."
        "RATE_LIMITED" -> "Слишком много запросов. Повторите позже."
        "CHECKOUT_POLICY_DENIED" -> "Оплата отклонена платёжной политикой."
        "OPERATION_PENDING" -> "Операция ещё выполняется. Проверьте статус позже."
        "OPERATION_FAILED" -> "Сервис сообщил об ошибке операции. Повторите позже."
        "OPERATION_UNKNOWN" -> "Состояние операции неизвестно. Проверьте статус позже."
        "ACCESS_SYNC_PENDING" -> "Доступ синхронизируется. Проверьте статус позже."
        else -> "Не удалось оформить покупку. Повторите позже."
    }

    /**
     * One visible purchase status line for the existing subscription surface. It only
     * renders accepted projection data or bounded fixed policy text; a paid-but-not-yet-
     * reflected purchase stays pending until the fresh /me projects an active right.
     */
    fun purchaseStatus(
        state: PurchaseState?,
        registration: AccountAccessProjection.Registration?,
        zone: ZoneId,
    ): String {
        val offered = state != null && PurchaseFlow.offered(registration)
        if (state == null || state.phase == PurchaseFlow.IDLE) {
            return if (offered) CHECK_AVAILABILITY_TEXT else PaymentsText.UNAVAILABLE_TEXT
        }
        // One outstanding purchase request: show waiting and let no other line imply progress.
        if (state.sending) return PURCHASE_SENDING_TEXT
        return when (state.phase) {
            PurchaseFlow.UNAVAILABLE -> PaymentsText.errorText(state.error)
            PurchaseFlow.ERROR -> {
                val payment = state.payment
                if (payment != null) listOfNotNull(
                    paymentStatusText(payment),
                    applicationStateText(payment.accessApplicationState),
                ).joinToString(" ") else PaymentsText.errorText(state.error)
            }
            PurchaseFlow.PLANS -> if (state.plans.isEmpty()) "Тарифы не загружены." else
                "Выберите тариф и способ оплаты."
            PurchaseFlow.QUOTE_READY -> state.quote?.let { "Предложение: ${quoteLine(it, zone)}" }
                ?: "Предложение не получено."
            PurchaseFlow.AWAITING_PAYMENT -> {
                val payment = state.payment
                if (payment == null) "Ожидаем оплату."
                else listOfNotNull(
                    paymentStatusText(payment),
                    applicationStateText(payment.accessApplicationState),
                ).joinToString(" ")
            }
            PurchaseFlow.AWAITING_CONFIRMATION ->
                if (PurchaseFlow.paidAwaitingBinding(state) && registration?.state != "registered")
                    "Оплата получена. Зарегистрируйтесь в Telegram, чтобы применить доступ."
                else "Оплата получена. Ожидаем подтверждение подписки по серверу…"
            PurchaseFlow.CONFIRMED -> "Оплата подтверждена сервером. Подписка обновлена."
            else -> if (offered) CHECK_AVAILABILITY_TEXT else PaymentsText.UNAVAILABLE_TEXT
        }
    }

    private fun groupDigits(value: Long): String {
        val digits = value.toString()
        val grouped = StringBuilder()
        for (index in digits.indices) {
            if (index > 0 && (digits.length - index) % 3 == 0) grouped.append(' ')
            grouped.append(digits[index])
        }
        return grouped.toString()
    }
}

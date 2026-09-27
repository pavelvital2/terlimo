package xyz.terlimo.test

/** One correlated list outcome: ignored (foreign/late) or accepted, with an optional stop. */
internal enum class DevicesListEffect { IGNORED, ACCEPTED, ACCEPTED_STOP }

/** Host display state of the §§18–19 connected-devices block; server-owned truth only. */
internal data class DevicesUi(
    val loaded: Boolean = false,
    val revision: Long? = null,
    val deviceLimit: Int = 0,
    val slotsUsed: Int = 0,
    val devices: List<DeviceRow> = emptyList(),
    // Outstanding list read: host-owned request token plus the attempt/account it belongs to.
    val listRequestId: String? = null,
    val listAttempt: String? = null,
    val listAccountRef: String? = null,
    // Identity of the attempt/account whose accepted list produced the current rows/slots.
    val loadedAttempt: String? = null,
    val loadedAccountRef: String? = null,
    // Single-flight delete: host-owned request id, target device, and its attempt/account.
    val pendingRequestId: String? = null,
    val pendingDeviceId: String? = null,
    val pendingAttempt: String? = null,
    val pendingAccountRef: String? = null,
    // Last accepted delete result, kept/displayed separately from any list refresh.
    val lastDelete: DeviceDeleteResult? = null,
    val error: String? = null,
) {
    val deleteInFlight: Boolean get() = pendingRequestId != null
}

/**
 * Display/ordering policy of the devices block. It never derives eligibility or a limit, never
 * deletes more than the one explicitly chosen device and never treats a `pending` deletion as an
 * applied revoke. A list read applies only with its exact host-owned token plus the same attempt
 * and (when known) the same account identity, so a late/foreign list is dropped. sessionGeneration
 * is deliberately NOT used as an account fence: it is a reused counter, not an identity.
 */
internal object DevicesPolicy {
    /** Management is offered only after a confirmed Telegram proof (server registration). */
    fun canManage(registration: AccountAccessProjection.Registration?): Boolean =
        registration?.state == "registered"

    /** A send is allowed when no foreign list token is outstanding; the own token is reusable. */
    fun canSendList(current: DevicesUi?, requestId: String): Boolean =
        current?.listRequestId == null || current.listRequestId == requestId

    /** A send is allowed when no foreign delete in-flight; the own host-owned request is reusable. */
    fun canSendDelete(current: DevicesUi?, requestId: String): Boolean =
        current?.pendingRequestId == null || current.pendingRequestId == requestId

    /** Records the outstanding list read. */
    fun beginList(current: DevicesUi?, requestId: String, attempt: String, accountRef: String?): DevicesUi =
        (current ?: DevicesUi()).copy(
            listRequestId = requestId, listAttempt = attempt, listAccountRef = accountRef, error = null)

    /**
     * Strict correlation of a list reply: exact host token, same attempt and the SAME nullable
     * account identity (a null list account is not a wildcard after a scope change).
     */
    private fun listCorrelated(ui: DevicesUi, requestId: String?, attempt: String, accountRef: String?): Boolean =
        ui.listRequestId != null && ui.listRequestId == requestId && ui.listAttempt == attempt &&
            ui.listAccountRef == accountRef

    /**
     * The single list decision covering BOTH the publish and the stop side effect. A foreign/late
     * reply is IGNORED and can never trigger a stop, even when it carries DEVICE_REMOVED.
     */
    fun listEffect(current: DevicesUi?, requestId: String?, attempt: String, accountRef: String?,
        list: DevicesList): DevicesListEffect {
        val base = current ?: DevicesUi()
        if (!listCorrelated(base, requestId, attempt, accountRef)) return DevicesListEffect.IGNORED
        return if (currentUnavailable(base.copy(devices = list.devices))) DevicesListEffect.ACCEPTED_STOP
        else DevicesListEffect.ACCEPTED
    }

    /** Failure variant of the same single decision (only a correlated DEVICE_REMOVED stops). */
    fun listFailureEffect(current: DevicesUi?, requestId: String?, attempt: String, accountRef: String?,
        code: String): DevicesListEffect {
        val base = current ?: DevicesUi()
        if (!listCorrelated(base, requestId, attempt, accountRef)) return DevicesListEffect.IGNORED
        return if (code == "DEVICE_REMOVED") DevicesListEffect.ACCEPTED_STOP else DevicesListEffect.ACCEPTED
    }

    /** Applies an accepted list only when it correlates; otherwise the reply is ignored. */
    fun applyList(current: DevicesUi?, requestId: String?, attempt: String, accountRef: String?,
        list: DevicesList): DevicesUi {
        val base = current ?: DevicesUi()
        if (!listCorrelated(base, requestId, attempt, accountRef)) return base
        val incoming = list.revision.toLongOrNull()
        if (incoming != null && base.revision != null && incoming < base.revision) {
            return base.copy(listRequestId = null)
        }
        return base.copy(
            loaded = true,
            revision = incoming ?: base.revision,
            deviceLimit = list.deviceLimit,
            slotsUsed = list.slotsUsed,
            devices = list.devices,
            listRequestId = null, listAttempt = null, listAccountRef = null,
            loadedAttempt = attempt,
            loadedAccountRef = accountRef,
            error = null,
        )
    }

    /**
     * A correlated list failure/malformed reply keeps any unrelated delete in flight untouched
     * and never releases it; it only surfaces the list error.
     */
    fun applyListFailure(current: DevicesUi?, requestId: String?, attempt: String, accountRef: String?,
        code: String): DevicesUi {
        val base = current ?: DevicesUi()
        if (!listCorrelated(base, requestId, attempt, accountRef)) return base
        return base.copy(listRequestId = null, listAttempt = null, listAccountRef = null, error = code)
    }

    /** Correlated list error clears the list token only; pending delete fields are preserved. */
    fun listFailureIsRemoved(code: String): Boolean = code == "DEVICE_REMOVED"

    fun beginDelete(current: DevicesUi?, requestId: String, deviceId: String, attempt: String,
        accountRef: String?): DevicesUi = (current ?: DevicesUi()).copy(
        pendingRequestId = requestId, pendingDeviceId = deviceId,
        pendingAttempt = attempt, pendingAccountRef = accountRef, error = null)

    fun deleteCorrelated(ui: DevicesUi, requestId: String?, attempt: String, accountRef: String?): Boolean =
        ui.pendingRequestId != null && ui.pendingRequestId == requestId && ui.pendingAttempt == attempt &&
            ui.pendingAccountRef == accountRef

    /** Delete uses the same single decision shape for its own side effects. */
    fun deleteEffect(ui: DevicesUi, requestId: String?, attempt: String, accountRef: String?,
        code: String?): DevicesListEffect = when {
        !deleteCorrelated(ui, requestId, attempt, accountRef) -> DevicesListEffect.IGNORED
        code == "DEVICE_REMOVED" -> DevicesListEffect.ACCEPTED_STOP
        else -> DevicesListEffect.ACCEPTED
    }

    /**
     * Records one correlated delete result. Slots/rows are NOT changed here: the server list is
     * re-read afterwards (slot_released and an applied revoke are independent), so the count is
     * never decremented twice or guessed.
     */
    fun applyDelete(current: DevicesUi, result: DeviceDeleteResult): DevicesUi =
        current.copy(pendingRequestId = null, pendingDeviceId = null, lastDelete = result, error = null)

    /** Releases the in-flight delete only for the matching request id (never a foreign one). */
    fun releaseDelete(current: DevicesUi, requestId: String?, error: String?): DevicesUi =
        if (requestId != null && current.pendingRequestId == requestId) {
            current.copy(pendingRequestId = null, pendingDeviceId = null, error = error)
        } else current

    fun reset(): DevicesUi = DevicesUi()

    /**
     * The one durable delete outcome line. A pending/not_requested receipt never claims the
     * gateway-side revoke was applied: the list does not carry that evidence. Only an
     * authoritative applied result states it; a confirmed slot release is reported separately.
     */
    fun deleteStateText(result: DeviceDeleteResult?): String? = when (result?.accessApplicationState) {
        null -> null
        "applied" -> "Устройство удалено. Доступ по нему отозван."
        "pending" ->
            if (result.slotReleased) "Запрос на удаление принят сервером.\nСлот освобождён."
            else "Запрос на удаление принят сервером."
        "retryable_failure" -> "Сервер не смог применить удаление и повторит попытку."
        "rejected" -> "Сервер отклонил удаление устройства."
        "not_requested" -> if (result.slotReleased) "Слот освобождён." else null
        else -> null
    }

    /**
     * The subscription-usage line for the same account and attempt: the correlated devices
     * list wins over the older /me projection, whose slots may predate an accepted change.
     * A foreign/attempt-mismatched cache is never used; the caller keeps its own fallback.
     */
    fun reconciledDeviceLine(devices: DevicesUi?, activeAttempt: String?, accountRef: String?,
        currentActive: Boolean, fallback: String?): String? {
        val list = devices ?: return fallback
        if (!list.loaded || list.loadedAttempt == null || list.loadedAttempt != activeAttempt) return fallback
        if (!currentActive || list.loadedAccountRef != accountRef) return fallback
        if (list.deviceLimit <= 0) return fallback
        return "Устройства: ${list.slotsUsed} из ${list.deviceLimit}"
    }

    fun countLine(ui: DevicesUi?): String? =
        if (ui == null || !ui.loaded) null else "Устройства: ${ui.slotsUsed} из ${ui.deviceLimit}"

    /** A revoked current device makes the paid right unavailable for this exact device. */
    fun currentUnavailable(ui: DevicesUi?): Boolean =
        ui != null && ui.devices.any { it.isCurrent && it.status == "revoked" }

    /** Binding: free server slot and no ACTIVE current device (a revoked current counts absent). */
    fun canBind(ui: DevicesUi?): Boolean =
        ui != null && ui.loaded && ui.devices.none { it.isCurrent && it.status == "active" } &&
            ui.slotsUsed < ui.deviceLimit

    fun errorText(code: String?): String? = when (code) {
        null -> null
        "DEVICE_REMOVED" -> "Для этого устройства подписка недоступна."
        "DEVICE_MANAGEMENT_FORBIDDEN" -> "Управление устройствами откроется после подтверждения Telegram."
        "DEVICE_NOT_FOUND" -> "Устройство не найдено. Обновите список."
        "REVISION_CONFLICT" -> "Список устарел. Повторите после обновления."
        "DEVICE_DELETE_REJECTED" -> "Сервер отклонил удаление устройства."
        "SERVICE_UNAVAILABLE", "TRANSPORT" -> "Сервис устройств недоступен. Повторите позже."
        else -> null
    }
}

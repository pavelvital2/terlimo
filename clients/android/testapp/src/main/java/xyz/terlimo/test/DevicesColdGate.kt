package xyz.terlimo.test

internal sealed class DevicesRequest {
    abstract val requestId: String

    data class List(override val requestId: String) : DevicesRequest()

    data class Delete(override val requestId: String, val deviceId: String,
        val idempotencyKey: String) : DevicesRequest()
}

internal sealed class DevicesVerified {
    object NotCold : DevicesVerified()
    object Refused : DevicesVerified()
    data class Send(val request: DevicesRequest) : DevicesVerified()
}

internal enum class DevicesTapAction { SEND_NOW, START_SERVICE, IGNORE, ERROR }

internal class DevicesColdGate {
    private var startPending = false
    private var coldAttempt: String? = null
    private var pending: DevicesRequest? = null
    private var sentAttempt: String? = null
    private var coldConsumed = false

    @Synchronized
    fun onTap(attemptId: String?, canManage: Boolean, request: DevicesRequest): DevicesTapAction {
        if (startPending || coldAttempt != null || pending != null || sentAttempt != null) return DevicesTapAction.IGNORE
        if (!canManage) return DevicesTapAction.ERROR
        if (attemptId == null) {
            startPending = true
            pending = request
            return DevicesTapAction.START_SERVICE
        }
        sentAttempt = attemptId
        return DevicesTapAction.SEND_NOW
    }

    @Synchronized
    fun onAttemptStarted(attemptId: String): Boolean {
        if (!startPending) return false
        startPending = false
        coldAttempt = attemptId
        coldConsumed = false
        return true
    }

    @Synchronized
    fun onVerifiedRights(attemptId: String, canManage: Boolean): DevicesVerified {
        if (coldAttempt != attemptId) return DevicesVerified.NotCold
        if (coldConsumed || pending == null) return DevicesVerified.NotCold
        if (!canManage) return DevicesVerified.Refused
        val request = pending ?: return DevicesVerified.NotCold
        pending = null
        coldConsumed = true
        sentAttempt = attemptId
        return DevicesVerified.Send(request)
    }

    @Synchronized
    fun isCold(attemptId: String?): Boolean = coldAttempt != null && coldAttempt == attemptId

    @Synchronized
    fun onRefused(attemptId: String): Boolean {
        if (coldAttempt != attemptId) return false
        clear()
        return true
    }

    @Synchronized
    fun onResult(attemptId: String?): Boolean {
        if (attemptId == null) return false
        val wasCold = coldAttempt == attemptId
        if (sentAttempt == attemptId || wasCold) {
            clear()
            return wasCold
        }
        return false
    }

    @Synchronized
    fun onTimeout(attemptId: String?): Boolean {
        if (startPending && attemptId == null) {
            clear()
            return true
        }
        if (coldAttempt != null && coldAttempt == attemptId) {
            clear()
            return true
        }
        return false
    }

    @Synchronized
    fun reset() = clear()

    private fun clear() {
        startPending = false
        coldAttempt = null
        pending = null
        sentAttempt = null
        coldConsumed = false
    }
}

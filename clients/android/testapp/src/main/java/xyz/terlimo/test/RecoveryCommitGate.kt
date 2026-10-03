package xyz.terlimo.test

/** Cancelling and the actual durable write share one lock; a late result cannot enter the write. */
internal class RecoveryCommitGate {
    private var foregroundEpoch = 0L
    private var currentAttempt: String? = null
    private var currentInstallation: String? = null

    @Synchronized
    fun begin(attempt: String, installation: String) {
        foregroundEpoch = 0L
        currentAttempt = attempt
        currentInstallation = installation
    }

    @Synchronized
    fun cancel() {
        currentAttempt = null
        currentInstallation = null
    }

    @Synchronized
    fun writeIfCurrent(attempt: String, installation: String, write: () -> Boolean): Boolean {
        if (currentAttempt != attempt || currentInstallation != installation) return false
        return write()
    }

    /** Invalidate queued optional writes before a foreground command enters the actor. */
    @Synchronized
    fun advanceForeground(): Long { foregroundEpoch++; return foregroundEpoch }

    @Synchronized
    fun writeOptionalIfCurrent(attempt: String, installation: String, epoch: Long,
        write: () -> Boolean): Boolean {
        if (epoch < 0 || epoch != foregroundEpoch) return false
        return writeIfCurrent(attempt, installation, write)
    }

    companion object {
        fun foregroundCommand(type: String): Boolean = type in setOf(
            "device_sleep", "device_wake", "cancel", "select_node", "explicit_connect",
            "request_telegram_registration", "refresh_telegram_registration", "activate_trial",
            "referral_info", "referral_candidate_set", "referral_candidate_clear",
            "plans_list", "quote_create", "payment_create", "payment_get", "usage_read",
            "announcements_list", "announcement_read", "devices_list", "device_delete",
            "cancel_catalog", "refresh_manual", "switch_node", "choose_node", "probe_node", "cancel_probe")
    }
}

package xyz.terlimo.test

/**
 * Navigation must never wake the service for a command that has no work: leaving HOME only
 * cancels an actually running common ping, and HELP only asks an existing attempt for the
 * announcement list. SessionService keeps the defensive counterpart so a stray idle intent
 * can neither promote an empty foreground instance nor stop live work.
 */
internal object NavigationServiceCommands {
    const val CANCEL_COMMON_PING = "probe_all_cancel"
    const val REQUEST_ANNOUNCEMENTS = "announcements_request"

    fun shouldCancelCommonPing(leavingHome: Boolean, commonPingActive: Boolean): Boolean =
        leavingHome && commonPingActive

    fun shouldRequestAnnouncements(openedHelp: Boolean, attemptActive: Boolean): Boolean =
        openedHelp && attemptActive

    fun isIdleCommand(action: String?): Boolean =
        action == CANCEL_COMMON_PING || action == REQUEST_ANNOUNCEMENTS
}

package xyz.terlimo.test

/**
 * Minimal display-permission policy for the functional VPN notification (§07.4).
 *
 * Android 13 (API 33) made notifications a runtime permission; without it the
 * foreground-service notification is not shown. Request it at most once per install
 * (persisted), never gate Connect on the answer, and stay fully usable if denied: the
 * tunnel/data plane never depends on it and the in-app status/diagnostics remain.
 */
internal object NotificationPermission {
    const val REQUEST_CODE = 0x7e71
    const val PREFS = "terlimo-permissions"
    const val ASKED_KEY = "post_notifications_asked"

    fun shouldRequest(sdkInt: Int, granted: Boolean, alreadyAsked: Boolean): Boolean =
        sdkInt >= 33 && !granted && !alreadyAsked
}

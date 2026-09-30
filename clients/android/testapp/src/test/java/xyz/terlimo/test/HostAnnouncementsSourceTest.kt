package xyz.terlimo.test

import java.io.File
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * S5 §11 host wiring guard: the native vocabulary is consumed by SessionService, the Help
 * surface renders «Уведомления» with a red dot and the notification carries the deep link,
 * so a later refactor cannot silently drop the wiring. No device/ADB involved.
 */
class HostAnnouncementsSourceTest {
    private fun source(path: String): String =
        listOf(File(path), File("testapp/$path"), File("../$path")).first { it.isFile }.readText()

    private val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
    private val activity = source("src/main/java/xyz/terlimo/test/MainActivity.kt")

    @Test
    fun `service consumes the announcements events and sends the read marker`() {
        assertTrue(service.contains("\"announcements_list\" ->"))
        assertTrue(service.contains("\"announcement_read\" ->"))
        assertTrue(service.contains("AnnouncementsCodec.parseList(event)"))
        assertTrue(service.contains("AnnouncementsCodec.parseRead(event)"))
        // The HELP navigation now sends the fixed command through the shared constant.
        assertTrue(activity.contains("NavigationServiceCommands.REQUEST_ANNOUNCEMENTS"))
        assertTrue(service.contains("private fun requestAnnouncements()"))
        assertTrue(service.contains("private fun openAnnouncement(announcementId: String)"))
        assertTrue(service.contains(".put(\"type\", \"announcement_read\")"))
        assertTrue(service.contains(".put(\"idempotency_key\", request.key)"))
    }

    @Test
    fun `help section shows unread red dot and the read action, denied note, deep link`() {
        assertTrue(activity.contains("Уведомления"))
        assertTrue(activity.contains("AnnouncementsPolicy.unreadCount"))
        assertTrue(activity.contains("AnnouncementsText.deniedPermissionNote"))
        assertTrue(activity.contains(".setAction(\"announcement_read\")"))
        assertTrue(activity.contains("AnnouncementDeepLink.shouldOpen"))
        // The unread state is bound to the persistent Help tab button, not only the header.
        assertTrue(activity.contains("helpTabButton?.text = BottomNavigation.unreadLabel(\"Помощь\", NavTarget.HELP, unread)"))
        // Order matters: the map must be populated before the Help button is captured, or
        // the reference stays null at the first render. Compare positions, not presence.
        val populateAt = activity.indexOf("destinationButtons[navDestination.target] = button")
        val bindAt = activity.indexOf("helpTabButton = destinationButtons[NavTarget.HELP]")
        assertTrue("map population line missing", populateAt >= 0)
        assertTrue("help binding line missing", bindAt >= 0)
        assertTrue("helpTabButton must be bound AFTER destinationButtons is populated",
            bindAt > populateAt)
        assertTrue(service.contains("AnnouncementDeepLink.EXTRA_OPEN_ANNOUNCEMENTS"))
        // The VPN Disconnect action must stay intact next to the open-announcements extra.
        assertTrue(service.contains("\"Отключить\""))
    }

    @Test
    fun `manual refresh uses one active-aware path, not the resume no-op`() {
        // SessionService must handle "refresh": one bounded native cycle when an attempt
        // is active, standard bootstrap when idle.
        assertTrue(service.contains("\"refresh\" ->"))
        assertTrue(service.contains("native?.send(JSONObject().put(\"type\", \"refresh_manual\"))"))
        assertTrue(service.contains("submitControl { begin(\"\") }"))
        // The catalog refresh surfaces and the news refresh_catalog action use "refresh".
        assertTrue((Regex("""setAction\("refresh"\)""").findAll(activity).count()) >= 3)
        assertTrue(activity.contains("AnnouncementTask.REFRESH_CATALOG -> startService(Intent(this@MainActivity, SessionService::class.java).setAction(\"refresh\"))"))
        // The old no-op wiring for the news action must be gone.
        assertFalse(activity.contains("AnnouncementTask.REFRESH_CATALOG -> startService(Intent(this@MainActivity, SessionService::class.java).setAction(\"resume\"))"))
    }
}
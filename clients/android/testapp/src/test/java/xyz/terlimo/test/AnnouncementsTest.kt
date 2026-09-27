package xyz.terlimo.test

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class AnnouncementsContractTest {
    private fun listEvent(vararg items: String, unreadCount: Int = items.size) =
        JSONObject(
            """{"v":1,"attempt_id":"a","type":"announcements_list","state":"ok",
               "request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-19T15:20:00Z",
               "schema_version":"1.0","unread_count":$unreadCount,
               "announcements":[${items.joinToString(",")}]}""".trimIndent())

    private fun item(id: String = "an-1", extra: String = "") =
        """{"announcement_id":"$id","title":"t","text":"x","action":{"type":"none"},"unread":true,"revision":"1","valid_until":null$extra}"""

    @Test
    fun `parses an ok list frame`() {
        val snapshot = AnnouncementsCodec.parseList(listEvent(item()))
        assertEquals(1, snapshot.unreadCount)
        assertEquals("an-1", snapshot.announcements.single().id)
        assertEquals(AnnouncementAction.NONE, snapshot.announcements.single().action.type)
        assertNull(snapshot.announcements.single().validUntil)
    }

    @Test
    fun `rejects malformed list frames`() {
        val bad = listOf(
            listEvent(item(extra = """, "unknown":1""")),
            listEvent(item().replace(""""title":"t",""", "")),
            listEvent(item().replace(""""unread":true""", """"unread":null""")),
            listEvent(item().replace(""""text":"x"""", """"text":null""")),
            listEvent(item().replace(""""type":"none"""", """"type":"explode"""")),
            listEvent(item().replace(""""revision":"1"""", """"revision":"01"""")),
            listEvent(item(), unreadCount = -1),
            listEvent(item()).put("extra", true).toString().let { JSONObject(it) },
        )
        for (raw in bad) {
            assertTrue("accepted malformed: $raw", runCatching { AnnouncementsCodec.parseList(raw) }.isFailure)
        }
    }

    @Test
    fun `accepts empty title and text and rejects JSON type coercion`() {
        val empty = listEvent(item().replace(""""title":"t"""", """"title":""""").replace(""""text":"x"""", """"text":"""""))
        val parsed = AnnouncementsCodec.parseList(empty)
        assertEquals("", parsed.announcements.single().title)
        assertEquals("", parsed.announcements.single().text)
        val base = listEvent(item()).toString()
        val coerced = listOf(
            JSONObject(base).put("unread_count", "1").toString(),
            base.replace("\"unread\":true", "\"unread\":\"true\""),
            base.replace("\"revision\":\"1\"", "\"revision\":1"),
        )
        for (raw in coerced) {
            assertTrue("accepted coercion: $raw", runCatching { AnnouncementsCodec.parseList(JSONObject(raw)) }.isFailure)
        }
    }

    @Test
    fun `parses read acknowledgement and rejects malformed ones`() {
        val ack = AnnouncementsCodec.parseRead(JSONObject(
            """{"v":1,"attempt_id":"a","type":"announcement_read","state":"ok",
               "request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-19T15:20:00Z",
               "schema_version":"1.0","announcement_id":"an-1","read":true,"read_at":"2026-09-19T15:21:00Z"}"""))
        assertTrue(ack.read)
        assertEquals("an-1", ack.announcementId)
        val bad = listOf(
            """{"state":"ok","schema_version":"1.0","announcement_id":"an-1","read":null,"read_at":"2026-09-19T15:21:00Z"}""",
            """{"state":"ok","schema_version":"1.0","announcement_id":"an-1","read":true}""",
            """{"state":"error","schema_version":"1.0","announcement_id":"an-1","read":true,"read_at":"2026-09-19T15:21:00Z"}""",
        )
        for (raw in bad) {
            assertTrue(runCatching { AnnouncementsCodec.parseRead(JSONObject(raw)) }.isFailure)
        }
    }

    @Test
    fun `expiry hides only past valid_until`() {
        val now = java.time.Instant.parse("2026-09-19T15:00:00Z").toEpochMilli()
        val past = Announcement("a", "t", "x", AnnouncementAction(AnnouncementAction.NONE, null),
            "2026-09-19T14:00:00Z", true, "1")
        val future = past.copy(id = "b", validUntil = "2026-09-19T16:00:00Z")
        val none = past.copy(id = "c", validUntil = null)
        val malformed = past.copy(id = "d", validUntil = "not-a-date")
        val visible = AnnouncementsCodec.visible(listOf(past, future, none, malformed), now).map { it.id }
        assertEquals(listOf("b", "c", "d"), visible)
    }

    @Test
    fun `unreserved path ids only`() {
        assertTrue(AnnouncementsCodec.unreservedPathId("an-1"))
        assertTrue(AnnouncementsCodec.unreservedPathId("A.b~c-1"))
        for (bad in listOf("", ".", "..", "a..b", "an:1", "ан-1", "a%2Fb", "a/b", "a b")) {
            assertFalse(bad, AnnouncementsCodec.unreservedPathId(bad))
        }
    }
}

class AnnouncementsPolicyTest {
    private fun snapshot(requestId: String, revision: String = "1", unread: Boolean = true, count: Int = 1) =
        AnnouncementsSnapshot(
            requestId, "2026-09-19T15:20:00Z",
            listOf(Announcement("an-1", "t", "x", AnnouncementAction(AnnouncementAction.NONE, null), null, unread, revision)),
            count,
        )

    @Test
    fun `identical snapshot does not churn state`() {
        val first = AnnouncementsPolicy.applyList(AnnouncementsUi(), snapshot("a"))
        val second = AnnouncementsPolicy.applyList(first, snapshot("a"))
        assertTrue(first === second)
    }

    @Test
    fun `same payload under a new envelope id does not republish`() {
        val first = AnnouncementsPolicy.applyList(AnnouncementsUi(), snapshot("a"))
        val second = AnnouncementsPolicy.applyList(first, snapshot("b"))
        assertTrue("a new request_id/time with identical content must not republish", first === second)
    }

    @Test
    fun `revision replacement applies and keeps local read set`() {
        var ui = AnnouncementsPolicy.applyList(AnnouncementsUi(), snapshot("a", revision = "1"))
        ui = AnnouncementsPolicy.beginRead(ui, "an-1") { "k-0000000000000001" }.first
        ui = AnnouncementsPolicy.ack(ui, AnnouncementReadAck("an-1", true, "2026-09-19T15:21:00Z"), ui.pending!!.token)
        assertFalse(AnnouncementsPolicy.redDot(ui, 0))
        val replaced = AnnouncementsPolicy.applyList(ui, snapshot("b", revision = "2"))
        assertEquals("2", replaced.snapshot!!.announcements.single().revision)
        assertFalse("a locally read message must not reappear unread", AnnouncementsPolicy.redDot(replaced, 0))
    }

    @Test
    fun `read retry reuses the key and new id gets a new key`() {
        var counter = 0
        val newKey = { "key-00000000000000" + (++counter) }
        var ui = AnnouncementsUi()
        val (afterBegin, request) = AnnouncementsPolicy.beginRead(ui, "an-1", newKey)
        ui = afterBegin
        assertEquals("key-000000000000001", request!!.key)
        ui = AnnouncementsPolicy.fail(ui, ui.pending!!.token)
        val (afterRetry, retry) = AnnouncementsPolicy.beginRead(ui, "an-1", newKey)
        ui = afterRetry
        assertEquals("retry must reuse the exact key", "key-000000000000001", retry!!.key)
        assertEquals(1, counter)
    }

    @Test
    fun `unread clears only on matching read true and a stale ack is ignored`() {
        var ui = AnnouncementsPolicy.applyList(AnnouncementsUi(), snapshot("a"))
        ui = AnnouncementsPolicy.beginRead(ui, "an-1") { "key-000000000000001" }.first
        val token = ui.pending!!.token
        // wrong id
        assertTrue(ui === AnnouncementsPolicy.ack(ui, AnnouncementReadAck("other", true, "t"), token))
        // read:false keeps unread
        val notRead = AnnouncementsPolicy.ack(ui, AnnouncementReadAck("an-1", false, "t"), token)
        assertTrue(AnnouncementsPolicy.redDot(notRead, 0))
        assertNull(notRead.pending)
        // a late ack for the already-cleared pending is a no-op
        assertTrue(notRead === AnnouncementsPolicy.ack(notRead, AnnouncementReadAck("an-1", true, "t"), token))
    }

    @Test
    fun `unreadable id is never sent and a second concurrent read is refused`() {
        val ui = AnnouncementsPolicy.applyList(AnnouncementsUi(), snapshot("a"))
        val unreadable = ui.copy(snapshot = snapshot("a").copy(
            announcements = listOf(Announcement("an:1", "t", "x", AnnouncementAction(AnnouncementAction.NONE, null), null, true, "1"))))
        assertNull(AnnouncementsPolicy.beginRead(unreadable, "an:1") { "key-000000000000001" }.second)
        val busy = AnnouncementsPolicy.beginRead(ui, "an-1") { "key-000000000000001" }.first
        assertNull(AnnouncementsPolicy.beginRead(busy, "an-1") { "key-000000000000002" }.second)
    }
}

class AnnouncementActionsTest {
    @Test
    fun `task mapping and honest surface availability`() {
        assertEquals(AnnouncementTask.REFRESH_CATALOG, AnnouncementActions.task(AnnouncementAction.REFRESH_CATALOG))
        assertEquals(AnnouncementTask.OPEN_PAYMENTS, AnnouncementActions.task(AnnouncementAction.OPEN_PAYMENTS))
        assertEquals(AnnouncementTask.OPEN_SUPPORT, AnnouncementActions.task(AnnouncementAction.OPEN_SUPPORT))
        assertEquals(AnnouncementTask.NONE, AnnouncementActions.task(AnnouncementAction.NONE))
        assertTrue(AnnouncementActions.enabled(AnnouncementTask.REFRESH_CATALOG, true, false))
        assertTrue(AnnouncementActions.enabled(AnnouncementTask.OPEN_PAYMENTS, true, false))
        assertFalse(AnnouncementActions.enabled(AnnouncementTask.OPEN_SUPPORT, true, false))
        assertFalse(AnnouncementActions.enabled(AnnouncementTask.NONE, true, true))
    }

    @Test
    fun `text helpers never invent a date or hide a missed system notification`() {
        assertEquals("без срока", AnnouncementsText.dateText(null))
        assertEquals("Обновить каталог", AnnouncementsText.actionLabel(AnnouncementAction(AnnouncementAction.REFRESH_CATALOG, null)))
        assertNull(AnnouncementsText.deniedPermissionNote(granted = true, hasVisibleMessage = true))
        assertNull(AnnouncementsText.deniedPermissionNote(granted = false, hasVisibleMessage = false))
        assertTrue(AnnouncementsText.deniedPermissionNote(granted = false, hasVisibleMessage = true)!!.contains("не показываются"))
        assertTrue(AnnouncementDeepLink.shouldOpen(true))
        assertFalse(AnnouncementDeepLink.shouldOpen(false))
    }

    @Test
    fun `help tab carries the unread red dot and no other tab does`() {
        assertEquals("Помощь ●", BottomNavigation.unreadLabel("Помощь", NavTarget.HELP, 3))
        assertEquals("Помощь", BottomNavigation.unreadLabel("Помощь", NavTarget.HELP, 0))
        assertEquals("Главная", BottomNavigation.unreadLabel("Главная", NavTarget.HOME, 3))
    }
}

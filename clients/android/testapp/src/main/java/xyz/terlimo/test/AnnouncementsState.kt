package xyz.terlimo.test

/**
 * Host-side S5 §11 announcements state and pure policy. Display-only: it never mutates
 * entitlement, catalog, VPN or native state. The host owns the read-marker Idempotency-Key;
 * the same key is reused for every retry of the same announcement, and unread/red dot are
 * cleared only on a matching `read:true` acknowledgement.
 */
internal data class PendingAnnouncementRead(val announcementId: String, val key: String, val token: Long)

internal data class AnnouncementReadRequest(val announcementId: String, val key: String)

internal data class AnnouncementsUi(
    val snapshot: AnnouncementsSnapshot? = null,
    val readIds: Set<String> = emptySet(),
    val readAt: Map<String, String> = emptyMap(),
    val keysById: Map<String, String> = emptyMap(),
    val pending: PendingAnnouncementRead? = null,
    val tokenSeq: Long = 0,
    val failedId: String? = null,
)

internal object AnnouncementsPolicy {
    /** Visible (non-expired) announcements that are still unread after local acks. */
    fun visibleUnread(ui: AnnouncementsUi, nowEpochMs: Long): List<Announcement> =
        AnnouncementsCodec.visible(ui.snapshot?.announcements.orEmpty(), nowEpochMs)
            .filter { it.unread && it.id !in ui.readIds }

    fun unreadCount(ui: AnnouncementsUi, nowEpochMs: Long): Int = visibleUnread(ui, nowEpochMs).size

    fun redDot(ui: AnnouncementsUi, nowEpochMs: Long): Boolean = unreadCount(ui, nowEpochMs) > 0

    /**
     * A repeated identical server snapshot must not produce a new UI state, so an
     * unchanged poll cannot churn the Help surface or re-post anything.
     */
    fun applyList(current: AnnouncementsUi, incoming: AnnouncementsSnapshot): AnnouncementsUi {
        // The envelope request_id/server_time change on every GET, so dedupe on the user
        // payload only: an unchanged feed must not republish the Help surface.
        val snapshot = current.snapshot
        if (snapshot != null && snapshot.announcements == incoming.announcements &&
            snapshot.unreadCount == incoming.unreadCount) {
            return current
        }
        return current.copy(snapshot = incoming, failedId = null)
    }

    /**
     * Starts one read attempt. Only one may be pending; a retry after a failure reuses the
     * exact same key bytes, and a new key is issued only when none is remembered for the id.
     */
    fun beginRead(
        current: AnnouncementsUi,
        announcementId: String,
        newKey: () -> String,
    ): Pair<AnnouncementsUi, AnnouncementReadRequest?> {
        if (current.pending != null) return current to null
        if (announcementId in current.readIds) return current to null
        if (!AnnouncementsCodec.unreservedPathId(announcementId)) return current to null
        val key = current.keysById[announcementId] ?: newKey()
        val token = current.tokenSeq + 1
        return current.copy(
            pending = PendingAnnouncementRead(announcementId, key, token),
            tokenSeq = token,
            keysById = current.keysById + (announcementId to key),
            failedId = null,
        ) to AnnouncementReadRequest(announcementId, key)
    }

    /**
     * Applies one `announcement_read` acknowledgement. A late/mismatched token or id never
     * clears a newer snapshot's unread state.
     */
    fun ack(current: AnnouncementsUi, ack: AnnouncementReadAck, token: Long): AnnouncementsUi {
        val pending = current.pending ?: return current
        if (pending.token != token || pending.announcementId != ack.announcementId) return current
        return if (ack.read) {
            current.copy(
                readIds = current.readIds + ack.announcementId,
                readAt = current.readAt + (ack.announcementId to ack.readAt),
                pending = null,
                keysById = current.keysById - ack.announcementId,
                failedId = null,
            )
        } else {
            // Server explicitly did not mark it read: stay unread, keep the key for a retry.
            current.copy(pending = null, failedId = ack.announcementId)
        }
    }

    /** A failed read keeps the announcement unread and its key reusable for the retry. */
    fun fail(current: AnnouncementsUi, token: Long): AnnouncementsUi {
        val pending = current.pending ?: return current
        if (pending.token != token) return current
        return current.copy(pending = null, failedId = pending.announcementId)
    }
}

/** The four fixed native action types (none is not actionable). */
internal enum class AnnouncementTask { NONE, REFRESH_CATALOG, OPEN_PAYMENTS, OPEN_SUPPORT }

internal object AnnouncementActions {
    fun task(type: String): AnnouncementTask = when (type) {
        AnnouncementAction.REFRESH_CATALOG -> AnnouncementTask.REFRESH_CATALOG
        AnnouncementAction.OPEN_PAYMENTS -> AnnouncementTask.OPEN_PAYMENTS
        AnnouncementAction.OPEN_SUPPORT -> AnnouncementTask.OPEN_SUPPORT
        else -> AnnouncementTask.NONE
    }

    /**
     * Only existing product surfaces are enabled. Payments reuses the existing
     * Subscription surface; support has no surface, so its action is honestly inactive
     * rather than a fabricated chat/checkout.
     */
    fun enabled(task: AnnouncementTask, paymentsSurface: Boolean, supportSurface: Boolean): Boolean =
        when (task) {
            AnnouncementTask.NONE -> false
            AnnouncementTask.REFRESH_CATALOG -> true
            AnnouncementTask.OPEN_PAYMENTS -> paymentsSurface
            AnnouncementTask.OPEN_SUPPORT -> supportSurface
        }
}

internal object AnnouncementsText {
    fun dateText(validUntil: String?): String = validUntil?.let { "до $it" } ?: "без срока"

    fun actionLabel(action: AnnouncementAction): String? = when (action.type) {
        AnnouncementAction.REFRESH_CATALOG -> action.label ?: "Обновить каталог"
        AnnouncementAction.OPEN_PAYMENTS -> action.label ?: "Подписка"
        AnnouncementAction.OPEN_SUPPORT -> action.label ?: "Поддержка"
        AnnouncementAction.NONE -> action.label
        else -> null
    }

    /**
     * Internal note shown only next to a visible current message when the runtime
     * notification permission is denied. It states the in-app reminder stays available; it
     * never claims a specific push was or was not delivered (the server does not prove that).
     */
    fun deniedPermissionNote(granted: Boolean, hasVisibleMessage: Boolean): String? =
        if (granted || !hasVisibleMessage) null else
            "Системные уведомления сейчас не показываются: разрешение выключено. Напоминание остаётся здесь."

    const val EMPTY = "Объявлений нет"

    /** Honest reason a message cannot be marked read with the current wire gate. */
    fun unreadableNote(): String =
        "Это объявление нельзя отметить прочитанным: недопустимый идентификатор."
}

/** The single host-side switch for the notification → «Уведомления» deep link. */
internal object AnnouncementDeepLink {
    const val EXTRA_OPEN_ANNOUNCEMENTS = "open_announcements"

    fun shouldOpen(extra: Boolean): Boolean = extra
}

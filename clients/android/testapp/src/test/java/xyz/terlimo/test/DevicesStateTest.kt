package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class DevicesStateTest {
    private fun row(id: String, current: Boolean, status: String = "active") =
        DeviceRow(id, "Name $id", "android", current, status, "2026-09-27T09:00:00Z", null)

    private fun list(vararg rows: DeviceRow, revision: String = "7", limit: Int = 2) =
        DevicesList(rows.toList(), deviceLimit = limit, slotsUsed = rows.size, revision = revision)

    private fun deleteApplied(req: String, state: String = "applied", released: Boolean = true) =
        DeviceDeleteResult(req, "ok", "op-1", released, state, null)

    private val attempt = "att-1"
    private val account = "acct-A"

    private fun loaded(vararg rows: DeviceRow, revision: String = "7", limit: Int = 2): DevicesUi =
        DevicesPolicy.applyList(
            DevicesPolicy.beginList(null, "devlist-0", attempt, account),
            "devlist-0", attempt, account, list(*rows, revision = revision, limit = limit))

    @Test fun managementNeedsConfirmedRegistration() {
        assertFalse(DevicesPolicy.canManage(null))
        assertFalse(DevicesPolicy.canManage(AccountAccessProjection.Registration("pending", false, false, null, true)))
        assertTrue(DevicesPolicy.canManage(AccountAccessProjection.Registration("registered", false, false, null, true)))
    }

    @Test fun listAppliesOnlyWhenTokenAttemptAndAccountMatch() {
        val pending = DevicesPolicy.beginList(null, "devlist-1", attempt, account)
        val applied = DevicesPolicy.applyList(pending, "devlist-1", attempt, account, list(row("a", true)))
        assertTrue(applied.loaded)
        // Late token, wrong attempt and foreign account are all ignored.
        assertEquals(pending, DevicesPolicy.applyList(pending, "devlist-0", attempt, account, list(row("z", true))))
        assertEquals(pending, DevicesPolicy.applyList(pending, "devlist-1", "att-2", account, list(row("z", true))))
        assertEquals(pending, DevicesPolicy.applyList(pending, "devlist-1", attempt, "acct-B", list(row("z", true))))
    }

    @Test fun listFailureOrMalformedNeverReleasesAnUnrelatedDelete() {
        val delete = DevicesPolicy.beginDelete(loaded(row("a", true), row("b", false)),
            "devdel-1", "b", attempt, account)
        val listPending = DevicesPolicy.beginList(delete, "devlist-2", attempt, account)
        // A correlated list error preserves the pending delete; only the list token is cleared.
        val failed = DevicesPolicy.applyListFailure(listPending, "devlist-2", attempt, account, "TRANSPORT")
        assertEquals("devdel-1", failed.pendingRequestId)
        assertNull(failed.listRequestId)
        assertEquals("TRANSPORT", failed.error)
        // A foreign/mismatched list error changes nothing at all.
        assertEquals(listPending, DevicesPolicy.applyListFailure(listPending, "devlist-9", attempt, account, "TRANSPORT"))
    }

    @Test fun deleteReleaseOnlyMatchesItsOwnRequestId() {
        val delete = DevicesPolicy.beginDelete(loaded(row("a", true), row("b", false)),
            "devdel-1", "b", attempt, account)
        assertEquals(delete, DevicesPolicy.releaseDelete(delete, "devdel-OTHER", "TRANSPORT"))
        val released = DevicesPolicy.releaseDelete(delete, "devdel-1", "TRANSPORT")
        assertNull(released.pendingRequestId)
        assertEquals("TRANSPORT", released.error)
    }

    @Test fun deleteCorrelationUsesRequestAttemptAndAccount() {
        val delete = DevicesPolicy.beginDelete(loaded(row("a", true), row("b", false)),
            "devdel-1", "b", attempt, account)
        assertTrue(DevicesPolicy.deleteCorrelated(delete, "devdel-1", attempt, account))
        assertFalse(DevicesPolicy.deleteCorrelated(delete, "devdel-1", "att-2", account))
        assertFalse(DevicesPolicy.deleteCorrelated(delete, "devdel-1", attempt, "acct-B"))
        assertFalse(DevicesPolicy.deleteCorrelated(delete, "devdel-OTHER", attempt, account))
    }

    @Test fun deleteResultRecordsOutcomeWithoutLocalSlotMath() {
        val base = DevicesPolicy.beginDelete(loaded(row("a", true), row("b", false)),
            "devdel-1", "b", attempt, account)
        val applied = DevicesPolicy.applyDelete(base, deleteApplied("devdel-1"))
        assertNull(applied.pendingRequestId)
        assertEquals("applied", applied.lastDelete?.accessApplicationState)
        assertEquals(2, applied.devices.size)
        assertEquals(2, applied.slotsUsed)
    }

    @Test fun listEffectIgnoresForeignAndStopsOnlyOnCorrelatedDevicesRemoved() {
        val revoked = list(row("a", true, "revoked"), revision = "8")
        val current = DevicesPolicy.beginList(loaded(row("a", true), row("b", false)), "devlist-1", attempt, account)
        // Foreign/late replies never produce a stop, even with DEVICE_REMOVED or a revoked row.
        assertEquals(DevicesListEffect.IGNORED,
            DevicesPolicy.listFailureEffect(current, "devlist-0", attempt, account, "DEVICE_REMOVED"))
        assertEquals(DevicesListEffect.IGNORED,
            DevicesPolicy.listFailureEffect(current, "devlist-1", "att-2", account, "DEVICE_REMOVED"))
        assertEquals(DevicesListEffect.IGNORED,
            DevicesPolicy.listFailureEffect(current, "devlist-1", attempt, "acct-B", "DEVICE_REMOVED"))
        assertEquals(DevicesListEffect.IGNORED,
            DevicesPolicy.listEffect(current, "devlist-0", attempt, account, revoked))
        assertEquals(DevicesListEffect.IGNORED,
            DevicesPolicy.listEffect(current, "devlist-1", attempt, "acct-B", revoked))
        // Correlated replies: a plain list is accepted, a revoked current row stops once.
        assertEquals(DevicesListEffect.ACCEPTED,
            DevicesPolicy.listEffect(current, "devlist-1", attempt, account, list(row("c", false), revision = "8")))
        assertEquals(DevicesListEffect.ACCEPTED_STOP,
            DevicesPolicy.listEffect(current, "devlist-1", attempt, account, revoked))
        assertEquals(DevicesListEffect.ACCEPTED_STOP,
            DevicesPolicy.listFailureEffect(current, "devlist-1", attempt, account, "DEVICE_REMOVED"))
    }

    @Test fun nullableAccountIsNotAWildcardForListOrDelete() {
        val nullScope = DevicesPolicy.beginList(null, "devlist-1", attempt, null)
        assertEquals(DevicesListEffect.IGNORED,
            DevicesPolicy.listEffect(nullScope, "devlist-1", attempt, "acct-B", list(row("a", true))))
        val knownScope = DevicesPolicy.beginList(null, "devlist-1", attempt, "acct-A")
        assertEquals(DevicesListEffect.IGNORED,
            DevicesPolicy.listEffect(knownScope, "devlist-1", attempt, null, list(row("a", true))))

        val deleteNull = DevicesPolicy.beginDelete(loaded(row("a", true), row("b", false)),
            "devdel-1", "b", attempt, null)
        assertFalse(DevicesPolicy.deleteCorrelated(deleteNull, "devdel-1", attempt, "acct-B"))
        val deleteKnown = DevicesPolicy.beginDelete(loaded(row("a", true), row("b", false)),
            "devdel-1", "b", attempt, "acct-A")
        assertFalse(DevicesPolicy.deleteCorrelated(deleteKnown, "devdel-1", attempt, null))
        assertEquals(DevicesListEffect.IGNORED,
            DevicesPolicy.deleteEffect(deleteKnown, "devdel-1", "att-2", "acct-A", "DEVICE_REMOVED"))
        assertEquals(DevicesListEffect.ACCEPTED_STOP,
            DevicesPolicy.deleteEffect(deleteKnown, "devdel-1", attempt, "acct-A", "DEVICE_REMOVED"))
    }

    @Test fun resetClearsEveryScopedField() {
        assertTrue(DevicesPolicy.reset() == DevicesUi())
        assertNull(DevicesPolicy.reset().listRequestId)
        assertNull(DevicesPolicy.reset().pendingRequestId)
        assertNull(DevicesPolicy.reset().lastDelete)
    }

    @Test fun canBindNeedsFreeSlotAndNoActiveCurrentEvenWhenCurrentRevoked() {
        assertTrue(DevicesPolicy.canBind(loaded(row("a", true, "revoked"), limit = 2)))
        assertFalse(DevicesPolicy.canBind(loaded(row("a", true), row("b", false))))
        assertFalse(DevicesPolicy.canBind(loaded(row("a", false), row("b", false))))
        assertTrue(DevicesPolicy.canBind(loaded(row("new", false), limit = 2)))
        assertFalse(DevicesPolicy.canBind(loaded(row("a", true), limit = 0)))
    }

    @Test fun coldDeleteKeepsItsOwnRequestAndRejectsForeignSingleFlight() {
        // The cold flow publishes nothing before the send, so the first own request is sendable.
        val empty = DevicesPolicy.reset()
        assertTrue(DevicesPolicy.canSendDelete(empty, "devdel-1"))
        assertTrue(DevicesPolicy.canSendList(empty, "devlist-1"))
        // The service records the own request exactly at the send; it stays reusable for that id…
        val own = DevicesPolicy.beginDelete(empty, "devdel-1", "dev-1", attempt, account)
        assertTrue(DevicesPolicy.canSendDelete(own, "devdel-1"))
        // …while a foreign in-flight request is never overwritten or doubled.
        assertFalse(DevicesPolicy.canSendDelete(own, "devdel-2"))
        val ownList = DevicesPolicy.beginList(empty, "devlist-1", attempt, account)
        assertTrue(DevicesPolicy.canSendList(ownList, "devlist-1"))
        assertFalse(DevicesPolicy.canSendList(ownList, "devlist-2"))
    }

    @Test fun refusalClearsOnlyTheOwnedRequestAndKeepsUiHonest() {
        val own = DevicesPolicy.beginDelete(DevicesPolicy.reset(), "devdel-1", "dev-1", attempt, account)
        val released = DevicesPolicy.releaseDelete(own, "devdel-1", "DEVICE_MANAGEMENT_FORBIDDEN")
        assertFalse(released.deleteInFlight)
        assertEquals("DEVICE_MANAGEMENT_FORBIDDEN", released.error)
        assertEquals("Управление устройствами откроется после подтверждения Telegram.",
            DevicesPolicy.errorText(released.error))
        // A foreign release cannot touch the own pending request.
        val foreign = DevicesPolicy.releaseDelete(own, "devdel-9", "TRANSPORT")
        assertEquals(own, foreign)
    }

    @Test fun deleteStateTextMatchesCanonicalOutcomes() {
        val pending = DevicesPolicy.deleteStateText(DeviceDeleteResult(
            "r", "pending", "op", false, "pending", 600))!!
        assertTrue(pending.contains("Запрос на удаление принят сервером."))
        assertFalse(pending.contains("ещё"))
        assertFalse(pending.contains("applied", ignoreCase = true))
        assertEquals("Запрос на удаление принят сервером.\nСлот освобождён.",
            DevicesPolicy.deleteStateText(DeviceDeleteResult(
                "r", "pending", "op", true, "pending", 600)))
        assertEquals("Устройство удалено. Доступ по нему отозван.",
            DevicesPolicy.deleteStateText(deleteApplied("r", "applied")))
        assertEquals("Слот освобождён.",
            DevicesPolicy.deleteStateText(DeviceDeleteResult("r", "ok", "op", true, "not_requested", null)))
        assertNull(DevicesPolicy.deleteStateText(DeviceDeleteResult("r", "ok", "op", false, "not_requested", null)))
        assertTrue(DevicesPolicy.deleteStateText(DeviceDeleteResult(
            "r", "ok", "op", false, "rejected", null))!!.contains("отклонил"))
        assertTrue(DevicesPolicy.deleteStateText(DeviceDeleteResult(
            "r", "ok", "op", false, "retryable_failure", null))!!.contains("повторит"))
    }

    @Test fun attemptStopReleasesStuckTokensAndKeepsCorrelatedRows() {
        val loaded = DevicesPolicy.applyList(
            DevicesPolicy.beginList(null, "d1", attempt, account), "d1", attempt, account,
            DevicesList(listOf(row("a", true), row("b", false)), 2, 2, "7"))
        val pendingList = DevicesPolicy.beginList(loaded, "d2", attempt, account)
        // A token from a dead attempt would block every later send…
        assertFalse(DevicesPolicy.canSendList(pendingList, "d3"))
        val released = DevicesPolicy.releaseInFlight(pendingList, "SERVICE_UNAVAILABLE")
        assertTrue(DevicesPolicy.canSendList(released, "d3"))
        assertEquals("SERVICE_UNAVAILABLE", released.error)
        assertEquals(2, released.devices.size)
        assertEquals(7L, released.revision)
        assertEquals(attempt, released.loadedAttempt)
        val releasedKeepsError = DevicesPolicy.releaseInFlight(
            DevicesPolicy.beginList(loaded, "d4", attempt, account).copy(error = "DEVICE_NOT_FOUND"),
            "SERVICE_UNAVAILABLE")
        assertEquals("DEVICE_NOT_FOUND", releasedKeepsError.error)
        val pendingDelete = DevicesPolicy.beginDelete(loaded, "devdel-1", "b", attempt, account)
        assertFalse(DevicesPolicy.canSendDelete(pendingDelete, "devdel-2"))
        val releasedDelete = DevicesPolicy.releaseInFlight(pendingDelete, "SERVICE_UNAVAILABLE")
        assertTrue(DevicesPolicy.canSendDelete(releasedDelete, "devdel-2"))
        assertFalse(releasedDelete.deleteInFlight)
        assertNull(DevicesPolicy.releaseInFlight(null, "SERVICE_UNAVAILABLE").listRequestId)
    }

    @Test fun correlatedListSlotsWinOverOlderMeAndForeignCacheIsExcluded() {
        val fallback = "Устройства: 2 из 2"
        val first = DevicesPolicy.applyList(
            DevicesPolicy.beginList(null, "d1", attempt, account), "d1", attempt, account,
            DevicesList(listOf(row("a", true), row("b", false)), 2, 2, "7"))
        assertEquals("Устройства: 2 из 2",
            DevicesPolicy.reconciledDeviceLine(first, attempt, account, true, fallback))
        // The accepted delete + reconcile list moves the same account/attempt to 1 из 2.
        val second = DevicesPolicy.applyList(
            DevicesPolicy.beginList(first, "d2", attempt, account), "d2", attempt, account,
            DevicesList(listOf(row("a", true)), 2, 1, "8"))
        assertEquals("Устройства: 1 из 2",
            DevicesPolicy.reconciledDeviceLine(second, attempt, account, true, fallback))
        // Foreign attempt/account and a non-current projection fall back to the /me line.
        assertEquals(fallback, DevicesPolicy.reconciledDeviceLine(second, "att-2", account, true, fallback))
        assertEquals(fallback, DevicesPolicy.reconciledDeviceLine(second, attempt, "acct-B", true, fallback))
        assertEquals(fallback, DevicesPolicy.reconciledDeviceLine(second, attempt, account, false, fallback))
        assertEquals(fallback, DevicesPolicy.reconciledDeviceLine(null, attempt, account, true, fallback))
    }
}

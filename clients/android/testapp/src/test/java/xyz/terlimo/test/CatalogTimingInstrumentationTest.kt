package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test
import java.nio.file.Files
import java.nio.file.Paths
import java.util.TreeMap

/**
 * Execution tests of the secret-safe catalogue timing markers: every real ARM / rights DISARM /
 * ACCEPT / TIMEOUT path on the real [MobileCatalogGate] + [CatalogDeadlineTimer] pair emits the
 * corresponding record with an attempt correlation, monotonic elapsed since ARM and a UTC stamp.
 * A missing TIMEOUT is never treated as PASS: the timeout case is asserted positively.
 */
class CatalogTimingInstrumentationTest {
    private class VirtualClock {
        var now = 0L
            private set
        private val tasks = TreeMap<Long, MutableList<() -> Unit>>()

        fun schedule(delayMillis: Long, callback: () -> Unit) {
            tasks.getOrPut(now + delayMillis) { mutableListOf() }.add(callback)
        }

        fun advanceBy(millis: Long) {
            val target = now + millis
            while (true) {
                val next = tasks.firstEntry() ?: break
                if (next.key > target) break
                tasks.remove(next.key)
                now = next.key
                next.value.forEach { it() }
            }
            now = target
        }
    }

    private class Harness {
        val clock = VirtualClock()
        val gate = MobileCatalogGate()
        val markers = mutableListOf<CatalogTimerMarkerRecord>()
        val timeouts = mutableListOf<String>()
        var active: String? = null
        private var utc = 1_700_000_000_000L
        val timer = CatalogDeadlineTimer(
            schedule = clock::schedule,
            activeAttempt = { active },
            onTimeout = { timeouts += it },
            elapsed = { clock.now },
            utc = { utc += 1_000; utc },
            emit = { markers += it },
        )

        fun start(attempt: String) {
            active = attempt
            timer.apply(attempt, gate.onAttemptStart(attempt, true))
        }

        fun rights(dataAccess: String, attempt: String) {
            val action = gate.onAccountAccess(dataAccess, attempt)
            timer.apply(
                attempt,
                action,
                marker = if (action == MobileCatalogAction.DISARM) CatalogTimerMarker.DISARM else null,
                detail = "rights_$dataAccess",
            )
        }

        fun accept(attempt: String, revision: String) {
            timer.apply(
                attempt,
                gate.onCatalogAccepted(attempt),
                marker = CatalogTimerMarker.ACCEPT,
                detail = revision,
            )
        }
    }

    @Test
    fun armRecordsZeroElapsedAndAUtcStamp() {
        val h = Harness()
        h.start("aaaaaaaa-1111-2222-3333-444444444444")
        val marker = h.markers.single()
        assertEquals(CatalogTimerMarker.ARM, marker.marker)
        assertEquals("aaaaaaaa-1111-2222-3333-444444444444", marker.attempt)
        assertEquals(0L, marker.elapsedMs)
        assertTrue(marker.utcMs > 0)
        assertNull(marker.reason)
        assertNull(marker.revision)
    }

    @Test
    fun rightsTransitionDisarmsWithReasonAndMonotonicElapsed() {
        val h = Harness()
        h.start("bbbbbbbb-1111-2222-3333-444444444444")
        h.clock.advanceBy(2_500)
        h.rights("restricted_checkout", "bbbbbbbb-1111-2222-3333-444444444444")
        val marker = h.markers.last()
        assertEquals(CatalogTimerMarker.DISARM, marker.marker)
        assertEquals(2_500L, marker.elapsedMs)
        assertEquals("rights_restricted_checkout", marker.reason)
        assertNull(marker.revision)
    }

    @Test
    fun acceptRecordsTheRevisionAndNoTimeoutFollows() {
        val h = Harness()
        val attempt = "cccccccc-1111-2222-3333-444444444444"
        h.start(attempt)
        h.clock.advanceBy(4_000)
        h.accept(attempt, "22")
        val marker = h.markers.last()
        assertEquals(CatalogTimerMarker.ACCEPT, marker.marker)
        assertEquals(4_000L, marker.elapsedMs)
        assertEquals("22", marker.revision)
        h.clock.advanceBy(60_000)
        assertFalse(h.markers.any { it.marker == CatalogTimerMarker.TIMEOUT })
        assertTrue(h.timeouts.isEmpty())
    }

    @Test
    fun pendingWindowEmitsExactlyOneTimeoutMarker() {
        val h = Harness()
        val attempt = "dddddddd-1111-2222-3333-444444444444"
        h.start(attempt)
        h.clock.advanceBy(CatalogLoadTimeout.MILLIS)
        val timeout = h.markers.filter { it.marker == CatalogTimerMarker.TIMEOUT }
        assertEquals(1, timeout.size)
        assertEquals(attempt, timeout.single().attempt)
        assertEquals(CatalogLoadTimeout.MILLIS, timeout.single().elapsedMs)
        assertEquals(listOf(attempt), h.timeouts)
        h.clock.advanceBy(60_000)
        assertEquals(1, h.markers.count { it.marker == CatalogTimerMarker.TIMEOUT })
    }

    @Test
    fun clearDisarmsWithTheClearReason() {
        val h = Harness()
        h.start("eeeeeeee-1111-2222-3333-444444444444")
        h.clock.advanceBy(1_000)
        h.timer.clear()
        val marker = h.markers.last()
        assertEquals(CatalogTimerMarker.DISARM, marker.marker)
        assertEquals("clear", marker.reason)
        assertEquals(1_000L, marker.elapsedMs)
    }

    @Test
    fun supersededWindowTimesOutOnlyForTheCurrentAttempt() {
        val h = Harness()
        h.start("ffffffff-1111-2222-3333-444444444444")
        h.clock.advanceBy(1_000)
        h.start("99999999-1111-2222-3333-444444444444")
        h.clock.advanceBy(14_999)
        assertFalse(h.markers.any { it.marker == CatalogTimerMarker.TIMEOUT })
        h.clock.advanceBy(1)
        val timeout = h.markers.filter { it.marker == CatalogTimerMarker.TIMEOUT }
        assertEquals(1, timeout.size)
        assertEquals("99999999-1111-2222-3333-444444444444", timeout.single().attempt)
    }

    @Test
    fun markersAndSourceCarryNoAccountUrlOrSecret() {
        val h = Harness()
        val attempt = "abababab-1111-2222-3333-444444444444"
        h.start(attempt)
        h.clock.advanceBy(500)
        h.rights("none", attempt)
        h.start(attempt)
        h.accept(attempt, "20")
        for (marker in h.markers) {
            assertTrue(marker.reason == null || marker.reason.matches(Regex("rights_(none|restricted_checkout)|clear")))
            assertTrue(marker.revision == null || marker.revision.matches(Regex("[0-9]+")))
        }
        val service = String(
            Files.readAllBytes(Paths.get("src/main/java/xyz/terlimo/test/SessionService.kt")),
            Charsets.UTF_8,
        )
        val logStart = service.indexOf("private fun logCatalogMarker(")
        val logEnd = service.indexOf("\n    }", logStart)
        val logSite = service.substring(logStart, logEnd)
        assertTrue(logSite.contains("CatalogMarkerSanitizer.line(marker)"))
        assertTrue(logSite.contains("WDTT/Catalog"))
        for (forbidden in listOf("link", "mobile_base_url", "installation_id", "token", "password", "://")) {
            assertFalse("marker log must not carry $forbidden", logSite.contains(forbidden))
        }
        val sanitizer = String(
            Files.readAllBytes(Paths.get("src/main/java/xyz/terlimo/test/CatalogMarkerSanitizer.kt")),
            Charsets.UTF_8,
        )
        assertTrue(sanitizer.contains("[0-9]{1,19}"))
        assertTrue(sanitizer.contains("rights_none"))
        assertTrue(sanitizer.contains("\"invalid\""))
    }

    @Test
    fun hostileReasonRevisionAndAttemptAreSanitizedAtTheLogSite() {
        val hostile = CatalogMarkerSanitizer.line(
            CatalogTimerMarkerRecord(
                marker = CatalogTimerMarker.ACCEPT,
                attempt = "bad attempt\nX",
                elapsedMs = 5,
                utcMs = 6,
                reason = "rights_none\ninjected",
                revision = "22\r\ninjected",
            ),
        )
        assertFalse(hostile.contains("\n"))
        assertFalse(hostile.contains("\r"))
        assertFalse(hostile.contains("injected"))
        assertTrue(hostile.contains(":reason=invalid"))
        assertTrue(hostile.contains(":rev=invalid"))
        assertTrue(hostile.contains(":unknown"))
        val good = CatalogMarkerSanitizer.line(
            CatalogTimerMarkerRecord(
                marker = CatalogTimerMarker.ACCEPT,
                attempt = "abababab-1111-2222-3333-444444444444",
                elapsedMs = 4_000,
                utcMs = 1_700_000_000_000,
                reason = "rights_none",
                revision = "22",
            ),
        )
        assertTrue(good.contains(":reason=rights_none"))
        assertTrue(good.contains(":rev=22"))
        assertTrue(good.contains(":elapsed=4000"))
        assertTrue(good.contains(":utc=1700000000000"))
    }

    @Test
    fun acceptThenClearEmitsNoSecondDisarm() {
        val h = Harness()
        val attempt = "12121212-1111-2222-3333-444444444444"
        h.start(attempt)
        h.clock.advanceBy(3_000)
        h.accept(attempt, "20")
        h.timer.clear()
        assertEquals(0, h.markers.count { it.marker == CatalogTimerMarker.DISARM })
        assertEquals(1, h.markers.count { it.marker == CatalogTimerMarker.ACCEPT })
    }

    @Test
    fun timeoutThenClearEmitsNoDisarm() {
        val h = Harness()
        val attempt = "34343434-1111-2222-3333-444444444444"
        h.start(attempt)
        h.clock.advanceBy(CatalogLoadTimeout.MILLIS)
        h.timer.clear()
        assertEquals(1, h.markers.count { it.marker == CatalogTimerMarker.TIMEOUT })
        assertEquals(0, h.markers.count { it.marker == CatalogTimerMarker.DISARM })
    }

    @Test
    fun staleActionsOnAnAlreadyDisarmedWindowEmitNothing() {
        val h = Harness()
        val attempt = "56565656-1111-2222-3333-444444444444"
        h.start(attempt)
        h.accept(attempt, "20")
        val before = h.markers.size
        h.timer.apply(attempt, MobileCatalogAction.DISARM, CatalogTimerMarker.DISARM, "rights_none")
        h.timer.apply(attempt, MobileCatalogAction.DISARM, CatalogTimerMarker.ACCEPT, "21")
        h.timer.apply("99999999-1111-2222-3333-444444444444", MobileCatalogAction.DISARM, CatalogTimerMarker.DISARM, "rights_none")
        assertEquals(before, h.markers.size)
    }

    @Test
    fun rightsDisarmThenNewArmResetsTheElapsedBase() {
        val h = Harness()
        val attempt = "78787878-1111-2222-3333-444444444444"
        h.start(attempt)
        h.clock.advanceBy(2_000)
        h.rights("none", attempt)
        h.clock.advanceBy(1_000)
        h.rights("subscription_data", attempt)
        val arms = h.markers.filter { it.marker == CatalogTimerMarker.ARM }
        assertEquals(2, arms.size)
        assertEquals(0L, arms.last().elapsedMs)
        h.clock.advanceBy(4_000)
        h.accept(attempt, "23")
        val accept = h.markers.last()
        assertEquals(4_000L, accept.elapsedMs)
        assertEquals("23", accept.revision)
    }
}

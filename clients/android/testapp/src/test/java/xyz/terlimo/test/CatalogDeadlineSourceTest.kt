package xyz.terlimo.test

import java.nio.file.Files
import java.nio.file.Paths
import org.junit.Assert.*
import org.junit.Test

class CatalogDeadlineSourceTest {
    private fun source(path: String) = String(Files.readAllBytes(Paths.get(path)), Charsets.UTF_8)

    @Test fun deadlineIsArmedOnlyThroughTheInjectedTimerAndDisarmedOnAcceptance() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(service.contains("CatalogDeadlineTimer("))
        assertEquals(3, Regex("catalogTimer\\.apply\\(").findAll(service).count())
        assertFalse(service.contains("catalogDeadline"))
        val timer = source("src/main/java/xyz/terlimo/test/CatalogDeadlineTimer.kt")
        assertEquals(1, Regex("private fun arm\\(attempt: String\\)").findAll(timer).count())
        assertEquals(1, Regex("deadline\\.arm\\(attempt\\)").findAll(timer).count())
        val catalogHandler = service.substringAfter("\"catalog\" -> {")
            .substringBefore("\"node_probe_result\" ->")
        assertTrue(catalogHandler.contains("mobileCatalog.onCatalogAccepted(attempt)"))
    }

    @Test fun deadlineIsArmedBeforeAnyChildStartOrEmission() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        val armAt = service.indexOf("mobileCatalog.onAttemptStart(attempt, mobileBaseUrl != null)")
        val childStartAt = service.indexOf("child.start(start)")
        assertTrue("attempt-start transition must exist", armAt > 0)
        assertTrue("start transition must precede child start", armAt < childStartAt)
    }

    @Test fun deadlineCallbackUsesLifecycleNotAPhaseList() {
        val timer = source("src/main/java/xyz/terlimo/test/CatalogDeadlineTimer.kt")
        assertTrue(timer.contains(
            "CatalogLoadTimeout.shouldStop(activeAttempt(), attempt, deadline.pending(attempt))"))
        assertTrue(timer.contains("CatalogLoadTimeout.MILLIS)"))
        // No negative phase list: the decision is lifecycle-based, not phase-based.
        assertFalse(timer.contains("terminal = setOf"))
        assertFalse(timer.contains("\"CatalogReady\""))
        assertFalse(timer.contains("\"ConfiguringVPN\""))
    }

    @Test fun readinessDeadlineAndFailClosedAreUntouched() {
        val service = source("src/main/java/xyz/terlimo/test/SessionService.kt")
        assertTrue(service.contains("readinessProbe"))
        assertTrue(service.contains("probe.run("))
        val helper = source("src/main/java/xyz/terlimo/test/CatalogLoadTimeout.kt")
        assertTrue(helper.contains("MILLIS = 15_000L"))
    }
}

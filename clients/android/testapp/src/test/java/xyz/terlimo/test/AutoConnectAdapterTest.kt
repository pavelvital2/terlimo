package xyz.terlimo.test

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * §26.2 production adapter with fake ports: captured token/attempt fences, ready-catalog attach,
 * one shared deadline budget, timeouts as visible completions, Off/manual isolation.
 */
class AutoConnectAdapterTest {

    private class FakePorts : AutoConnectPorts {
        var activeGenerations = 5L
        var pref = true
        var defer = false
        val queue = java.util.ArrayDeque<() -> Unit>()
        fun drain() { while (queue.isNotEmpty()) queue.removeFirst().invoke() }
        override fun dispatch(action: () -> Unit) { if (defer) queue.add(action) else action() }
        override fun clearDeadline(attempt: String) { deadlineEnd = Long.MAX_VALUE }
        var gate: String? = null
        var stopping = false
        var phase = "CatalogReady"
        var nodes = listOf(NodeLabel("node-1", "Node"))
        var selected = ""
        var pending: String? = null
        var account: String? = "acc-1"
        var entitlement = true
        var consent = true
        var dataIntent = false
        var last: String? = "node-1"
        var manualPendingConnectOnCatalog = true

        val began = mutableListOf<String>()
        val chosen = mutableListOf<String>()
        val selectedSent = mutableListOf<String>()
        val errors = mutableListOf<String>()
        val terminated = mutableListOf<String>()
        val deadlineStarts = mutableListOf<String>()
        var timeoutDelay: Long? = null
        var timeoutCallback: (() -> Unit)? = null
        var clock = 0L
        private var deadlineEnd = Long.MAX_VALUE

        override fun activeGeneration(): Long = activeGenerations
        override fun prefEnabled(generation: Long): Boolean = pref && generation == activeGenerations
        override fun gateActive(): String? = gate
        override fun stopping(): Boolean = stopping
        override fun phase(): String = phase
        override fun nodes(): List<NodeLabel> = nodes
        override fun selectedNodeId(): String = selected
        override fun pendingNodeId(): String? = pending
        override fun accountRef(): String? = account
        override fun entitlementUsable(): Boolean = entitlement
        override fun vpnConsentGranted(): Boolean = consent
        override fun dataIntentActive(): Boolean = dataIntent || deadlineEnd != Long.MAX_VALUE
        override fun lastNodeId(accountRef: String): String? = last
        override fun beginAttempt() {
            began += (gate ?: "new")
            gate = gate ?: "A"
        }
        override fun sendChoose(nodeId: String) { chosen += nodeId }
        override fun sendSelect(nodeId: String, attempt: String) { selectedSent += nodeId }
        override fun startDeadline(attempt: String, now: Long): Boolean {
            deadlineStarts += attempt
            deadlineEnd = now + ConnectDeadline.MILLIS
            return true
        }
        override fun deadlineEnd(attempt: String): Long = deadlineEnd
        override fun armDeadlineTimeout(attempt: String, delayMillis: Long, onTimeout: () -> Unit) {
            timeoutDelay = delayMillis
            timeoutCallback = onTimeout
        }
        override fun publishError(code: String) { errors += code }
        override fun terminateAutoAttempt(attempt: String) { terminated += attempt }
        override fun now(): Long = clock
    }

    private fun adapter(ports: FakePorts) = AutoConnectAdapter(AutoConnectController(), ports)

    @Test fun readyCatalogAttachSendsSelectOnceWithOneBudget() {
        val ports = FakePorts().apply { gate = "A"; selected = "node-1" }
        val a = adapter(ports)
        a.onLaunch(5L)
        a.onCatalog("A")
        assertEquals(emptyList<String>(), ports.began)
        assertEquals(emptyList<String>(), ports.chosen)
        assertEquals(listOf("node-1"), ports.selectedSent)
        assertEquals(listOf("A"), ports.deadlineStarts)
        assertEquals(emptyList<String>(), ports.errors)
    }

    @Test fun chooseThenConfirmSharesOneDeadlineBudget() {
        val ports = FakePorts().apply { gate = "A"; selected = "other" }
        val a = adapter(ports)
        a.onLaunch(5L)
        a.onCatalog("A")
        assertEquals(listOf("node-1"), ports.chosen)
        assertEquals(listOf("A"), ports.deadlineStarts)
        assertEquals(ConnectDeadline.MILLIS, ports.timeoutDelay)
        ports.selected = "node-1"
        ports.clock = 5_000L
        a.onCatalog("A")
        assertEquals(listOf("node-1"), ports.selectedSent)
        assertEquals("one budget only", listOf("A"), ports.deadlineStarts)
    }

    @Test fun ackTimeoutIsAVisibleCompletionAndTerminatesAutoAttempt() {
        val ports = FakePorts().apply { gate = null; selected = "" }
        val a = adapter(ports)
        a.onLaunch(5L) // begins attempt "A"
        a.onCatalog("A") // choose
        ports.timeoutCallback?.invoke()
        assertTrue(ports.errors.contains(AutoConnectCode.TIMEOUT))
        assertEquals(listOf("A"), ports.terminated)
        assertEquals(emptyList<String>(), ports.selectedSent)
    }

    @Test fun staleGenerationEffectNeverTouchesNative() {
        val ports = FakePorts().apply { gate = "A"; selected = "other" }
        val controller = AutoConnectController()
        val a = AutoConnectAdapter(controller, ports)
        a.onLaunch(5L)
        val stale = controller.onCatalog(
            AutoConnectRuntime(true, 5L, true, "acc-1", "node-1", true, true, false), "other", false)
        ports.activeGenerations = 9L
        controller.disarm()
        a.applyForTest(stale, 5L, "A")
        assertEquals(emptyList<String>(), ports.chosen)
        assertEquals(emptyList<String>(), ports.selectedSent)
    }

    @Test fun staleAttemptEffectNeverTouchesNative() {
        val ports = FakePorts().apply { gate = "A"; selected = "other" }
        val controller = AutoConnectController()
        val a = AutoConnectAdapter(controller, ports)
        a.onLaunch(5L)
        val choose = controller.onCatalog(
            AutoConnectRuntime(true, 5L, true, "acc-1", "node-1", true, true, false), "other", false)
        ports.gate = "B"
        a.applyForTest(choose, 5L, "A")
        assertEquals(emptyList<String>(), ports.chosen)
    }

    @Test fun accountScopeMismatchDropsTheEffect() {
        val ports = FakePorts().apply { gate = "A"; selected = "other" }
        val controller = AutoConnectController()
        val a = AutoConnectAdapter(controller, ports)
        a.onLaunch(5L)
        val choose = controller.onCatalog(
            AutoConnectRuntime(true, 5L, true, "acc-1", "node-1", true, true, false), "other", false)
        ports.account = "acc-other"
        a.applyForTest(choose, 5L, "A")
        assertEquals(emptyList<String>(), ports.chosen)
    }

    @Test fun offCancelLeavesTheManualConnectUntouched() {
        val ports = FakePorts().apply { gate = "A"; selected = "other" }
        val a = adapter(ports)
        a.onLaunch(5L)
        a.cancel()
        assertTrue(ports.manualPendingConnectOnCatalog)
        assertEquals(emptyList<String>(), ports.terminated)
        assertEquals(emptyList<String>(), ports.errors)
        assertEquals(emptyList<String>(), ports.selectedSent)
    }

    @Test fun dataIntentLaunchConsumesWithoutStartingAnotherAttempt() {
        val ports = FakePorts().apply { gate = "A"; dataIntent = true }
        val a = adapter(ports)
        a.onLaunch(5L)
        assertEquals(emptyList<String>(), ports.began)
    }

    @Test fun staleGenerationLaunchIsIgnoredByTheServiceFence() {
        val ports = FakePorts().apply { gate = "A" }
        val a = adapter(ports)
        ports.activeGenerations = 9L
        a.onLaunch(8L)
        assertEquals(emptyList<String>(), ports.began)
        assertEquals(emptyList<String>(), ports.selectedSent)
    }

    @Test fun cancellationBetweenPlanAndWriterDropsChoose() {
        val p = FakePorts().apply { gate = "A"; selected = "other"; defer = true }
        val a = adapter(p)
        a.onLaunch(5); p.drain()
        a.onCatalog("A")
        a.cancel()
        p.drain()
        assertTrue(p.chosen.isEmpty())
        assertTrue(p.deadlineStarts.isEmpty())
    }

    @Test fun liveChangesBetweenPlanAndWriterDropSelect() {
        for (change in listOf<(FakePorts) -> Unit>(
            { it.pref = false }, { it.activeGenerations = 6 },
            { it.account = "other" }, { it.entitlement = false }, { it.gate = "B" },
            { it.consent = false }, { it.dataIntent = true })) {
            val p = FakePorts().apply { gate = "A"; selected = "node-1"; defer = true }
            val a = adapter(p)
            a.onLaunch(5); p.drain()
            a.onCatalog("A")
            change(p)
            p.drain()
            assertTrue(p.selectedSent.isEmpty())
        }
    }

    @Test fun canceledChooseReleasesItsBudgetForManualConnect() {
        val p = FakePorts().apply { gate = "A"; selected = "other" }
        val a = adapter(p)
        a.onLaunch(5); a.onCatalog("A")
        a.cancel()
        assertEquals(Long.MAX_VALUE, p.deadlineEnd("A"))
        assertTrue(p.manualPendingConnectOnCatalog)
        p.selected = "node-1"
        a.onCatalog("A")
        assertTrue(p.selectedSent.isEmpty())
    }

    @Test fun canceledQueuedColdStartDoesNotCreateAttempt() {
        val p = FakePorts().apply { defer = true }
        val a = adapter(p)
        a.onLaunch(5)
        a.cancel()
        p.drain()
        assertTrue(p.began.isEmpty())
    }

    @Test fun duplicateLaunchDuringOwnChooseDoesNotConsumeTheRun() {
        val p = FakePorts().apply { gate = "A"; selected = "other" }
        val a = adapter(p)
        a.onLaunch(5); a.onCatalog("A"); a.onLaunch(5)
        p.selected = "node-1"
        a.onCatalog("A")
        assertEquals(listOf("node-1"), p.selectedSent)
        assertEquals(listOf("node-1"), p.chosen)
    }
}

package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test
import java.io.File
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicReference

class BridgeActorTest {
    private fun await(latch: CountDownLatch) { assertTrue(latch.await(3, TimeUnit.SECONDS)) }

    @Test fun saturationHasEightQueuedPlusOneRunningAndExplicitTypeFailures() {
        val actor = BridgeActor(); val entered = CountDownLatch(1); val release = CountDownLatch(1)
        try {
            assertEquals(BridgeActor.Result.ACCEPTED, actor.submit { entered.countDown(); release.await() })
            await(entered)
            repeat(8) { assertEquals(BridgeActor.Result.ACCEPTED, actor.submit {}) }
            assertEquals(BridgeActor.Result.FULL, actor.submit {})
            val start = System.nanoTime()
            assertEquals(BridgeActor.Result.FULL, actor.submit(100) {})
            assertTrue(TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - start) >= 80)
            assertEquals("BRIDGE_SIGN_OVERFLOW", BridgeActor.overflowCode("sign"))
            assertEquals("BRIDGE_PERSIST_OVERFLOW", BridgeActor.overflowCode("persist"))
            assertEquals("BRIDGE_VPN_OVERFLOW", BridgeActor.overflowCode("vpn_config"))
            assertEquals("BRIDGE_INGRESS_OVERFLOW", BridgeActor.overflowCode("lease"))
        } finally { release.countDown(); actor.close() }
    }

    @Test fun backpressurePreservesPersistVpnOrderAndCompletionInsteadOfDropping() {
        val actor = BridgeActor(); val entered = CountDownLatch(1); val release = CountDownLatch(1)
        val all = CountDownLatch(9); val order = java.util.Collections.synchronizedList(mutableListOf<String>())
        val submitted = CountDownLatch(1); val result = AtomicReference<BridgeActor.Result>()
        try {
            actor.submit { entered.countDown(); release.await() }; await(entered)
            repeat(8) { i -> actor.submit { order.add(if (i == 0) "persist_ack" else "control-$i"); all.countDown() } }
            val producer = Thread { result.set(actor.submit(1000) { order.add("vpn_result"); all.countDown() }); submitted.countDown() }
            producer.start()
            assertFalse(submitted.await(50, TimeUnit.MILLISECONDS))
            release.countDown(); await(submitted); await(all); producer.join(1000)
            assertEquals(BridgeActor.Result.ACCEPTED, result.get())
            assertEquals("persist_ack", order.first()); assertEquals("vpn_result", order.last()); assertEquals(9, order.size)
        } finally { release.countDown(); actor.close() }
    }

    @Test fun cancelBypassesSaturationAndDropsLateActionsEvenIfRunningIgnoresInterrupt() {
        val actor = BridgeActor(); val gate = AttemptGate(); gate.start("old")
        val entered = CountDownLatch(1); val release = CountDownLatch(1); val finished = CountDownLatch(1)
        val ran = AtomicInteger(); val late = AtomicInteger(); val submitted = CountDownLatch(1)
        val result = AtomicReference<BridgeActor.Result>()
        try {
            gate.admit("old", "sign", 15000, 0)
            actor.submit {
                entered.countDown()
                while (release.count > 0) { try { release.await() } catch (_: InterruptedException) {} }
                if (gate.finish("old", "sign")) late.incrementAndGet()
                finished.countDown()
            }
            await(entered)
            repeat(8) { actor.submit { ran.incrementAndGet() } }
            val producer = Thread { result.set(actor.submit(1000) { ran.incrementAndGet() }); submitted.countDown() }
            producer.start()
            assertFalse(submitted.await(50, TimeUnit.MILLISECONDS))
            gate.cancel(); actor.close() // Neither needs the running task or a free queue slot.
            await(submitted)
            assertEquals(BridgeActor.Result.CLOSED, result.get())
            assertEquals(BridgeActor.Result.CLOSED, actor.submit { ran.incrementAndGet() })
            assertEquals(1L, finished.count) // close did not wait for this blocked task.
            assertFalse(actor.awaitStopped(50)) // Restart cannot race the old storage action.
            release.countDown(); await(finished); producer.join(1000)
            assertTrue(actor.awaitStopped(1000))
            assertEquals(0, ran.get()); assertEquals(0, late.get())
        } finally { release.countDown(); actor.close() }
    }

    private fun source(name: String): String = listOf(
        File("src/main/java/xyz/terlimo/test/$name.kt"),
        File("testapp/src/main/java/xyz/terlimo/test/$name.kt")
    ).first { it.isFile }.readText()

    @Test fun actualServiceUsesBoundedReaderAndNeverQueuesStopOrLosesRequiredReplySilently() {
        val service = source("SessionService")
        assertTrue(service.contains("private val actor = BridgeActor()"))
        assertFalse(service.contains("actor.execute"))
        val receive = service.substringAfter("private fun receiveBridge(").substringBefore("private fun send(")
        assertTrue(receive.contains("actor.submit(BridgeActor.BACKPRESSURE_MS)"))
        assertTrue(receive.contains("terminalFailure(attempt, BridgeActor.overflowCode(type))"))
        assertTrue(receive.indexOf("type == \"error\" || type == \"stopped\"") < receive.indexOf("actor.submit"))
        assertFalse(receive.contains("send(JSONObject")) // No possibly blocking refusal on reader.
        val stop = service.substringAfter("private fun stopAttempt(").substringBefore("override fun onDestroy()")
        assertTrue(stop.indexOf("gate.cancel()") < stop.indexOf("actor.close()"))
        assertTrue(stop.contains("Thread({")); assertTrue(stop.contains("\"terlimo-stop\""))
        assertTrue(stop.contains("child?.stop(")); assertFalse(stop.contains("actor.submit"))
        assertTrue(stop.indexOf("Tunnel.State.DOWN") < stop.indexOf("actor.awaitStopped(250)"))
        assertTrue(stop.indexOf("actor.awaitStopped(250)") < stop.indexOf("retiringActors.decrementAndGet()"))
        assertTrue(stop.indexOf("retiringActors.decrementAndGet()") < stop.indexOf("publish(SessionRetention.onStop("))
        assertTrue(service.contains("intent?.action != \"cancel\" && retiringActors.get() > 0"))
        assertTrue(service.contains("\"cancel\" -> stopAttempt(null)"))
        assertTrue(service.substringAfter("override fun onDestroy()").contains("stopAttempt(null)"))
        val persist = service.substringAfter("\"persist\" -> {").substringBefore("\"imported\" ->")
        assertTrue(persist.indexOf("storage.write(") < persist.indexOf("\"persist_result\""))
    }

    @Test fun productionBridgeAcceptsSyncAccessNativePhase() {
        val service = source("SessionService")
        val phases = service.substringAfter("private val NATIVE_PHASES = setOf(").substringBefore(")")
        assertTrue(phases.contains("\"SyncingAccess\""))
        val runner = listOf(
            File("../go_client/terlimo_runner.go"),
            File("go_client/terlimo_runner.go")
        ).first { it.isFile }.readText()
        assertTrue(runner.contains("c.stateContext(ctx, \"SyncingAccess\")"))
    }

    @Test fun nativeAbortDestroysBeforeTakingWriterMonitorAndChecksStartRace() {
        val native = source("NativeProcess")
        val stop = native.substringAfter("fun stop(")
        assertTrue(stop.indexOf("closing = true") < stop.indexOf("synchronized(this)"))
        assertTrue(stop.indexOf("child?.destroyForcibly()") < stop.indexOf("synchronized(this)"))
        assertFalse(stop.contains("send(JSONObject"))
        val start = native.substringAfter("fun start(").substringBefore("@Synchronized fun send(")
        assertTrue(start.indexOf("if (closing) return") < start.indexOf("spawn(executable.absolutePath)"))
        assertTrue(start.substringAfter("process = child").contains("if (closing) { child.destroyForcibly();"))
        assertTrue(start.substringAfter("process = child").contains("startExitWatcher(child)"))
        assertFalse(start.contains("finishChild"))
    }
}

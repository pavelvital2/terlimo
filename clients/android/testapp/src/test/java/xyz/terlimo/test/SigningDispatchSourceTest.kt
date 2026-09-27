package xyz.terlimo.test

import org.junit.Assert.*
import org.junit.Test
import java.io.File

/** Source contracts connect pure JVM gate tests to Android-only service dispatch.
 * These are not device/lifecycle or Android Keystore runtime tests.
 */
class SigningDispatchSourceTest {
    private fun source(name: String): String {
        val suffix = "src/main/java/xyz/terlimo/test/$name.kt"
        return listOf(File(suffix), File("testapp/$suffix")).first { it.isFile }.readText()
    }

    @Test fun realDispatchAdmitsBeforeSubmittingToSingleWorkerBoundedExecutor() {
        val service = source("SessionService")
        assertTrue(service.contains("ThreadPoolExecutor(1, 1, 0, TimeUnit.MILLISECONDS, ArrayBlockingQueue(4))"))
        val sign = service.substringAfter("private fun sign(").substringBefore("private fun armExpiry(")
        assertTrue(sign.indexOf("SigningPolicy.validateWorker(") < sign.indexOf("gate.admit("))
        assertTrue(sign.contains("if (!gate.admit(attempt, id, deadline, System.currentTimeMillis()))"))
        assertTrue(sign.substringAfter("if (!gate.admit").substringBefore("signer.execute").contains("return"))
        assertTrue(sign.contains("signer.execute {"))
        assertTrue(sign.contains("RejectedExecutionException"))
        assertTrue(sign.substringAfter("RejectedExecutionException").contains("gate.finish(attempt, id)"))
        assertTrue(sign.contains("SIGN_BUSY"))
        assertTrue(source("SigningPolicy").contains("pending.size >= 4"))
    }

    @Test fun realSignerChecksAttemptDeadlineAndDropsLateResults() {
        val sign = source("SessionService").substringAfter("private fun sign(").substringBefore("private fun armExpiry(")
        assertTrue(sign.contains("check(gate.active == attempt && System.currentTimeMillis() < deadline)"))
        assertTrue(sign.indexOf("check(gate.active == attempt") < sign.indexOf("storage.sign("))
        assertTrue(sign.contains("finally { transcript?.fill(0) }"))
        assertTrue(sign.contains("if (System.currentTimeMillis() >= deadline)"))
        assertTrue(sign.contains("reply.remove(\"signature_b64\"); reply.put(\"error\", \"SIGN_EXPIRED\")"))
        assertTrue(sign.contains("if (gate.finish(attempt, id)) runCatching { send(reply) }"))
        val storage = source("InstallationStore").substringAfter("fun sign(")
        assertTrue(storage.contains("SigningPolicy.validate(kind, transcript)"))
        assertTrue(storage.contains("Signature.getInstance(\"SHA256withECDSA\")"))
        assertTrue(storage.contains("update(transcript)"))
    }

    @Test fun realCancelInvalidatesBeforeNativeStopAndBlocksLateBridgeMessages() {
        val service = source("SessionService")
        val stop = service.substringAfter("private fun stopAttempt(").substringBefore("override fun onDestroy()")
        assertTrue(stop.indexOf("stopping.compareAndSet(false, true)") < stop.indexOf("gate.cancel()"))
        assertTrue(stop.indexOf("gate.cancel()") < stop.indexOf("child?.stop("))
        assertTrue(service.contains("if (!stopping.get() && gate.active != null) native?.send(message)"))
        assertTrue(service.contains("submitControl { if (gate.active == attempt && !stopping.get()) action() }"))
        assertTrue(stop.contains("signer.shutdownNow()"))
        val native = source("NativeProcess")
        assertTrue(native.contains("event.getInt(\"v\") == 1 && event.getString(\"attempt_id\") == attempt"))
        assertTrue(native.indexOf("BRIDGE_CORRELATION") < native.indexOf("onMessage(event)"))
        assertTrue(native.contains("if (closing) return"))
    }
}

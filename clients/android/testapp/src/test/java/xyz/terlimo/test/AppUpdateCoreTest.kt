package xyz.terlimo.test

import java.io.Closeable
import java.io.File
import java.net.HttpURLConnection
import java.net.URL
import java.security.MessageDigest
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.coroutines.EmptyCoroutineContext
import kotlinx.coroutines.*
import org.junit.Assert.*
import org.junit.Test

class AppUpdateCoreTest {
    private val deployment = AppUpdateDeployment("https://updates.example/downloads/stable.json", "test", "/downloads/")
    private fun json() = """{"schema":1,"channel":"test","packageName":"xyz.terlimo.test","versionCode":15,"versionName":"0.15","minSdk":28,"abi":"arm64-v8a","apkUrl":"https://updates.example/downloads/15.apk","sizeBytes":3,"sha256":"${"0".repeat(64)}"}"""
    private fun parse(raw: String = json()) = AppUpdateContract.parse(raw.toByteArray(), deployment)
    private fun rejected(block: () -> Unit) {
        try { block() } catch (_: Exception) { return }
        fail("invalid input accepted")
    }

    @Test fun strictMetadataRoundTripAndEligibility() {
        val release = parse(json().dropLast(1) + ",\"releaseNotes\":\"line\\nnext\"}")
        assertEquals("line\nnext", release.releaseNotes)
        assertTrue(AppUpdateContract.eligible(release, "xyz.terlimo.test", 14, 35, listOf("arm64-v8a")))
        assertFalse(AppUpdateContract.eligible(release, "com.wdtt.plus", 14, 35, listOf("arm64-v8a")))
        assertFalse(AppUpdateContract.eligible(release, "xyz.terlimo.test", 15, 35, listOf("arm64-v8a")))
        assertFalse(AppUpdateContract.eligible(release, "xyz.terlimo.test", 16, 35, listOf("arm64-v8a")))
        assertFalse(AppUpdateContract.eligible(release, "xyz.terlimo.test", 14, 27, listOf("arm64-v8a")))
        assertFalse(AppUpdateContract.eligible(release, "xyz.terlimo.test", 14, 35, listOf("x86_64")))
    }

    @Test fun rejectsCoercionDuplicatesUnknownAndNonJson() {
        for (number in listOf("\"15\"", "15.0", "1e2", "015", "true", "null", "9223372036854775808"))
            rejected { parse(json().replace("\"versionCode\":15", "\"versionCode\":$number")) }
        for (bad in listOf(json()+"{}", json().dropLast(1)+",}", json().replace("\"schema\":1", "\"schema\":1,\"schema\":1"),
            json().dropLast(1)+",\"unknown\":1}", json().replace("\"schema\"", "'schema'"),
            json().replace("\"versionCode\":15,", ""), json().replace("\"versionName\":\"0.15\"", "\"versionName\":\"bad\nname\"")))
            rejected { parse(bad) }
        rejected { AppUpdateContract.parse(byteArrayOf(0xc3.toByte(), 0x28), deployment) }
        rejected { AppUpdateContract.parse(ByteArray(65537), deployment) }
    }

    @Test fun rejectsUntrustedUrlsAndDeploymentChanges() {
        for (url in listOf("http://updates.example/downloads/15.apk", "https://evil.example/downloads/15.apk",
            "https://user@updates.example/downloads/15.apk", "https://updates.example:444/downloads/15.apk",
            "https://updates.example/other/15.apk", "https://updates.example/downloads/../15.apk",
            "https://updates.example/downloads/%2e%2e/15.apk", "https://updates.example/downloads/15.apk?q=1",
            "https://updates.example/downloads/15.apk#fragment")) {
            assertFalse(url, deployment.trustedUrl(url))
            rejected { parse(json().replace("https://updates.example/downloads/15.apk", url)) }
        }
        rejected { parse(json().replace("\"channel\":\"test\"", "\"channel\":\"stable\"")) }
        rejected { AppUpdateDeployment("http://updates.example/downloads/stable.json", "test", "/downloads/") }
    }

    @Test fun exactSizeAndHashAndCancellationDuringVerification() {
        val file = File.createTempFile("update-test", ".apk")
        try {
            file.writeBytes(byteArrayOf(1, 2, 3))
            val hash = MessageDigest.getInstance("SHA-256").digest(file.readBytes()).joinToString("") { "%02x".format(it) }
            val release = parse().copy(sha256 = hash)
            AppUpdateContract.verifyBytes(file, release)
            rejected { AppUpdateContract.verifyBytes(file, release.copy(sizeBytes = 2)) }
            rejected { AppUpdateContract.verifyBytes(file, release.copy(sha256 = "0".repeat(64))) }
            rejected { AppUpdateContract.verifyBytes(file, release) { throw CancellationException() } }
            for (size in listOf(0L, -1L, AppUpdateContract.MAX_APK + 1))
                rejected { AppUpdateContract.validate(release.copy(sizeBytes = size), deployment) }
        } finally { file.delete() }
    }

    @Test fun cancellationDisconnectsBlockedIoAndDeletesPartial() = runBlocking {
        val entered = CountDownLatch(1)
        val unblocked = CountDownLatch(1)
        val disconnected = AtomicBoolean(false)
        val closed = AtomicBoolean(false)
        val file = File.createTempFile("update-cancel", ".partial")
        val resultDelivered = AtomicBoolean(false)
        val task = launch {
            runUpdateOperation(5000) { operation ->
                operation.file(file)
                operation.connection(object : HttpURLConnection(URL("https://unused.invalid")) {
                    override fun connect() = Unit
                    override fun usingProxy() = false
                    override fun disconnect() { disconnected.set(true); unblocked.countDown() }
                })
                operation.stream(Closeable { closed.set(true) })
                entered.countDown()
                check(unblocked.await(2, TimeUnit.SECONDS))
                operation.check()
                file
            }
            resultDelivered.set(true)
        }
        withContext(Dispatchers.IO) { check(entered.await(2, TimeUnit.SECONDS)) }
        task.cancelAndJoin()
        assertTrue(disconnected.get()); assertTrue(closed.get())
        assertFalse(file.exists()); assertFalse(resultDelivered.get())
    }

    @Test fun totalDeadlineCancelsAndLateFileRegistrationIsRejected() = runBlocking {
        val file = File.createTempFile("update-timeout", ".partial")
        val disconnected = CountDownLatch(1)
        try {
            runUpdateOperation(100) { operation ->
                operation.file(file)
                operation.connection(object : HttpURLConnection(URL("https://unused.invalid")) {
                    override fun connect() = Unit
                    override fun usingProxy() = false
                    override fun disconnect() { disconnected.countDown() }
                })
                check(disconnected.await(2, TimeUnit.SECONDS))
                operation.check()
            }
            fail("deadline returned success")
        } catch (_: TimeoutCancellationException) { }
        assertFalse(file.exists())
        val cancelled = UpdateOperation(EmptyCoroutineContext, 1000)
        cancelled.abort()
        val late = File.createTempFile("update-late", ".partial")
        rejected { cancelled.file(late) }
        assertFalse(late.exists())
    }
}

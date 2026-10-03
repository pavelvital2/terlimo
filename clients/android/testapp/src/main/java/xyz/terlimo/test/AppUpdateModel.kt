package xyz.terlimo.test

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withTimeout
import java.io.Closeable
import java.net.HttpURLConnection
import kotlin.coroutines.CoroutineContext
import java.io.File
import java.net.URI
import java.nio.ByteBuffer
import java.nio.charset.CodingErrorAction
import java.security.MessageDigest

/** Static release contract. No account, recovery URL or intent may supply this configuration. */
internal data class AppUpdateDeployment(val manifestUrl: String, val channel: String, val pathPrefix: String) {
    init {
        require(channel.matches(Regex("[a-z0-9-]{1,32}")))
        require(pathPrefix.startsWith("/") && pathPrefix.endsWith("/") &&
            !pathPrefix.contains("..") && !pathPrefix.contains('%') && !pathPrefix.contains('\\'))
        require(trustedUrl(manifestUrl))
    }
    fun trustedUrl(value: String): Boolean = runCatching {
        val base = URI(manifestUrl)
        val uri = URI(value)
        fun valid(u: URI) = u.scheme == "https" && !u.host.isNullOrEmpty() &&
            u.rawUserInfo == null && (u.port == -1 || u.port == 443) &&
            u.rawQuery == null && u.rawFragment == null && u.rawPath.startsWith(pathPrefix) &&
            !u.rawPath.contains('%') && !u.rawPath.contains('\\') &&
            u.rawPath.split('/').none { it == "." || it == ".." }
        valid(base) && valid(uri) && base.host.equals(uri.host, ignoreCase = true)
    }.getOrDefault(false)
}

internal data class AppUpdateRelease(
    val channel: String, val packageName: String, val versionCode: Long, val versionName: String,
    val minSdk: Int, val abi: String, val apkUrl: String, val sizeBytes: Long, val sha256: String,
    val releaseNotes: String? = null
)

internal object AppUpdateContract {
    const val MAX_METADATA = 64 * 1024
    const val MAX_APK = 200L * 1024 * 1024

    /** Deliberately flat JSON schema: strings and decimal integers only. Reject duplicates,
     * unknown fields, fractional/coerced numbers, relaxed JSON and trailing documents. */
    fun parse(bytes: ByteArray, deployment: AppUpdateDeployment): AppUpdateRelease {
        require(bytes.size in 1..MAX_METADATA)
        val text = Charsets.UTF_8.newDecoder().onMalformedInput(CodingErrorAction.REPORT)
            .onUnmappableCharacter(CodingErrorAction.REPORT).decode(ByteBuffer.wrap(bytes)).toString()
        val fields = FlatMetadata(text).read()
        val required = setOf("schema", "channel", "packageName", "versionCode", "versionName",
            "minSdk", "abi", "apkUrl", "sizeBytes", "sha256")
        require(fields.keys.containsAll(required) && (fields.keys - required - "releaseNotes").isEmpty())
        fun string(key: String) = fields[key] as? String ?: error("UPDATE_METADATA_TYPE")
        fun integer(key: String) = fields[key] as? Long ?: error("UPDATE_METADATA_TYPE")
        require(integer("schema") == 1L)
        val sdk = integer("minSdk")
        require(sdk in 1..Int.MAX_VALUE.toLong())
        return AppUpdateRelease(string("channel"), string("packageName"), integer("versionCode"),
            string("versionName"), sdk.toInt(), string("abi"), string("apkUrl"), integer("sizeBytes"),
            string("sha256"), if (fields.containsKey("releaseNotes")) string("releaseNotes") else null)
            .also { validate(it, deployment) }
    }

    fun validate(r: AppUpdateRelease, d: AppUpdateDeployment) {
        require(r.channel == d.channel && r.versionCode > 0 && r.minSdk > 0)
        require(r.packageName.length <= 255 && r.packageName.matches(Regex("[A-Za-z][A-Za-z0-9_]*(\\.[A-Za-z][A-Za-z0-9_]*)+")))
        require(r.versionName.isNotBlank() && r.versionName.length <= 128 && r.versionName.none { it.code < 32 })
        require(r.abi == "arm64-v8a" && r.sizeBytes in 1..MAX_APK)
        require(r.sha256.matches(Regex("[0-9a-f]{64}")))
        require(r.apkUrl.length <= 2048 && d.trustedUrl(r.apkUrl) && URI(r.apkUrl).path.endsWith(".apk"))
        require(r.releaseNotes == null || r.releaseNotes.length <= 4096)
    }

    fun eligible(r: AppUpdateRelease, packageName: String, installedCode: Long, sdk: Int, abis: List<String>) =
        r.packageName == packageName && r.versionCode > installedCode && sdk >= r.minSdk && r.abi in abis

    fun verifyBytes(file: File, r: AppUpdateRelease, checkpoint: () -> Unit = {}) {
        require(file.isFile && file.length() == r.sizeBytes && r.sizeBytes in 1..MAX_APK)
        val digest = MessageDigest.getInstance("SHA-256")
        var total = 0L
        file.inputStream().use { input ->
            val buffer = ByteArray(32 * 1024)
            while (true) {
                checkpoint()
                val n = input.read(buffer)
                if (n < 0) break
                total += n
                require(total <= r.sizeBytes)
                digest.update(buffer, 0, n)
            }
        }
        checkpoint()
        require(total == r.sizeBytes && digest.digest().joinToString("") { "%02x".format(it) } == r.sha256)
    }
}

/** Small decoder for the above scalar-only schema; not a general JSON framework. */
private class FlatMetadata(private val text: String) {
    private var at = 0
    private fun space() { while (at < text.length && text[at] in " \r\n\t") at++ }
    private fun take(c: Char) { space(); require(at < text.length && text[at++] == c) }
    private fun string(): String {
        take('"')
        val out = StringBuilder()
        while (at < text.length) {
            val c = text[at++]
            if (c == '"') return out.toString()
            require(c.code >= 32)
            if (c != '\\') { out.append(c); continue }
            require(at < text.length)
            when (val escaped = text[at++]) {
                '"', '\\', '/' -> out.append(escaped)
                'b' -> out.append('\b')
                'f' -> out.append('\u000c')
                'n' -> out.append('\n')
                'r' -> out.append('\r')
                't' -> out.append('\t')
                'u' -> {
                    require(at + 4 <= text.length)
                    val hex = text.substring(at, at + 4)
                    require(hex.all { it in "0123456789abcdefABCDEF" })
                    out.append(hex.toInt(16).toChar()); at += 4
                }
                else -> error("UPDATE_METADATA_ESCAPE")
            }
        }
        error("UPDATE_METADATA_STRING")
    }
    fun read(): Map<String, Any> {
        take('{'); space()
        val result = linkedMapOf<String, Any>()
        if (at < text.length && text[at] == '}') { at++; space(); require(at == text.length); return result }
        while (true) {
            val key = string(); require(!result.containsKey(key)); take(':'); space()
            require(at < text.length)
            val value: Any = if (text[at] == '"') string() else {
                val start = at
                while (at < text.length && text[at] !in ",} \r\n\t") at++
                val token = text.substring(start, at)
                require(token.matches(Regex("-?(0|[1-9][0-9]*)")))
                token.toLong()
            }
            result[key] = value; space(); require(at < text.length)
            when (text[at++]) { '}' -> break; ',' -> Unit; else -> error("UPDATE_METADATA_DELIMITER") }
        }
        space(); require(at == text.length)
        return result
    }
}

@OptIn(ExperimentalCoroutinesApi::class)
internal suspend fun <T> runUpdateOperation(timeout: Long, block: (UpdateOperation) -> T): T = withTimeout(timeout) {
        suspendCancellableCoroutine { continuation ->
            val resources = UpdateOperation(continuation.context, timeout)
            continuation.invokeOnCancellation { resources.abort() }
            CoroutineScope(continuation.context).launch(Dispatchers.IO) {
                try {
                    resources.check()
                    val result = block(resources)
                    resources.check()
                    continuation.resume(result, onCancellation = { resources.abort() })
                } catch (error: Throwable) {
                    resources.abort()
                    continuation.resumeWith(Result.failure(error))
                } finally { resources.closeNetwork() }
            }
        }
    }

/** Registration/cancellation serialized; no partial survives cancellation racing file creation
 * or rename. Disconnect + stream close actively interrupt IO, in addition to socket timeouts. */
internal class UpdateOperation(private val context: CoroutineContext, timeoutMs: Long) {
    private val deadline = System.nanoTime() + timeoutMs * 1_000_000L
    private var cancelled = false
    private var connection: HttpURLConnection? = null
    private var stream: Closeable? = null
    private var file: File? = null
    fun check() {
        context.ensureActive()
        synchronized(this) { check(!cancelled) { "UPDATE_CANCELLED" } }
        check(System.nanoTime() < deadline) { "UPDATE_TIMEOUT" }
    }
    @Synchronized fun connection(value: HttpURLConnection) {
        if (cancelled) { value.disconnect(); error("UPDATE_CANCELLED") }
        connection = value
    }
    @Synchronized fun stream(value: Closeable) {
        if (cancelled) { value.close(); error("UPDATE_CANCELLED") }
        stream = value
    }
    @Synchronized fun file(value: File) {
        if (cancelled) { value.delete(); error("UPDATE_CANCELLED") }
        file = value
    }
    @Synchronized fun rename(from: File, to: File) {
        check(); require(from.renameTo(to)); file = to
    }
    fun abort() {
        val abandoned = synchronized(this) { cancelled = true; file }
        // Unlink first: a concurrent blocked writer can never leave an installable path.
        abandoned?.delete()
        closeNetwork()
    }
    fun closeNetwork() {
        val pair = synchronized(this) { val p = connection to stream; connection = null; stream = null; p }
        runCatching { pair.first?.disconnect() }
        runCatching { pair.second?.close() }
    }
}

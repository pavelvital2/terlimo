package xyz.terlimo.test

import android.content.Context
import android.content.Intent
import android.content.pm.PackageInfo
import android.content.pm.PackageManager
import android.os.Build
import androidx.core.content.FileProvider
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL
import java.security.MessageDigest
import java.util.zip.ZipFile

/** Adapted from com/wdtt/plus/AppUpdate.kt at 6f8d33d: bounded streaming, hash/signers,
 * FileProvider installer intent. No GitHub resolver, same-version fix or UI scheduling.
 * Caller checks after EACH successful user connection and manually from Settings;
 * this core has no throttle, auto-download, installer launch, account or VPN actions. */
internal class AppUpdate(private val deployment: AppUpdateDeployment) {
    companion object {
        const val METADATA_TIMEOUT_MS = 15_000L
        const val DOWNLOAD_TIMEOUT_MS = 5 * 60_000L
        const val VERIFY_TIMEOUT_MS = 30_000L
        private const val IO_TIMEOUT_MS = 5_000
    }

    suspend fun fetch(): AppUpdateRelease = runUpdateOperation(METADATA_TIMEOUT_MS) { io ->
        val connection = connect(deployment.manifestUrl, io, "application/json")
        val length = connection.contentLengthLong
        require(length <= AppUpdateContract.MAX_METADATA)
        val raw = ByteArrayOutputStream()
        val input = connection.inputStream.also { io.stream(it) }
        input.use {
            val buffer = ByteArray(4096)
            while (true) {
                io.check()
                val n = it.read(buffer)
                if (n < 0) break
                require(raw.size() + n <= AppUpdateContract.MAX_METADATA)
                raw.write(buffer, 0, n)
            }
        }
        io.check()
        AppUpdateContract.parse(raw.toByteArray(), deployment)
    }

    fun eligible(context: Context, release: AppUpdateRelease): Boolean {
        AppUpdateContract.validate(release, deployment)
        val installed = installed(context)
        return AppUpdateContract.eligible(release, context.packageName, installed.longVersionCode,
            Build.VERSION.SDK_INT, Build.SUPPORTED_ABIS.toList())
    }

    /** Private unique file; incomplete/cancelled work is deleted. No Range resume. Progress
     * executes on IO: caller posts to UI and must not block or perform account/VPN work. */
    suspend fun download(context: Context, release: AppUpdateRelease,
        progress: (downloaded: Long, total: Long) -> Unit = { _, _ -> }): File =
        runUpdateOperation(DOWNLOAD_TIMEOUT_MS) { io ->
            require(eligible(context, release)) { "UPDATE_NOT_ELIGIBLE" }
            val directory = File(context.cacheDir, "updates")
            require(directory.isDirectory || directory.mkdirs())
            val file = File.createTempFile("update-", ".partial", directory)
            io.file(file)
            val connection = connect(release.apkUrl, io, "application/vnd.android.package-archive")
            val length = connection.contentLengthLong
            require(length == -1L || length == release.sizeBytes) { "UPDATE_SIZE" }
            val input = connection.inputStream.also { io.stream(it) }
            var count = 0L
            input.use { source ->
                file.outputStream().use { output ->
                    val buffer = ByteArray(32 * 1024)
                    var lastProgress = 0L
                    while (true) {
                        io.check()
                        val n = source.read(buffer)
                        if (n < 0) break
                        count += n
                        require(count <= release.sizeBytes && count <= AppUpdateContract.MAX_APK)
                        output.write(buffer, 0, n)
                        val now = System.nanoTime()
                        if (now - lastProgress >= 250_000_000L) {
                            io.check(); progress(count, release.sizeBytes); lastProgress = now
                        }
                    }
                    output.flush()
                }
            }
            require(count == release.sizeBytes) { "UPDATE_SIZE" }
            verify(context, file, release, io::check)
            io.check()
            // Only verified files receive .apk. Keep cancellation cleanup pointed at the new name.
            val ready = File(file.parentFile, file.name.removeSuffix(".partial") + ".apk")
            io.rename(file, ready)
            io.check(); progress(count, release.sizeBytes); io.check()
            ready
        }

    /** Recheck on permission-return/process-resume and immediately before handing to Android.
     * Caller persists only this small update workflow, never identity/payment namespaces. */
    suspend fun verifyDownloaded(context: Context, file: File, release: AppUpdateRelease) {
        runUpdateOperation(VERIFY_TIMEOUT_MS) { io -> verify(context, file, release, io::check) }
    }

    fun canRequestInstall(context: Context): Boolean = context.packageManager.canRequestPackageInstalls()

    /** Constructs only; caller owns user confirmation/permission and launch. Authority must
     * be its manifest-declared, non-exported FileProvider exposing ONLY cache/updates/. */
    suspend fun installerIntent(context: Context, file: File, release: AppUpdateRelease,
        authority: String): Intent {
        require(authority.isNotBlank())
        verifyDownloaded(context, file, release)
        require(canRequestInstall(context)) { "UPDATE_INSTALL_PERMISSION" }
        return Intent(Intent.ACTION_VIEW).apply {
            setDataAndType(FileProvider.getUriForFile(context, authority, file),
                "application/vnd.android.package-archive")
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
        }
    }

    private fun verify(context: Context, file: File, r: AppUpdateRelease, checkpoint: () -> Unit) {
        require(file.canonicalFile.parentFile == File(context.cacheDir, "updates").canonicalFile)
        require(eligible(context, r)) { "UPDATE_NOT_ELIGIBLE" }
        AppUpdateContract.verifyBytes(file, r, checkpoint)
        val archive = context.packageManager.getPackageArchiveInfo(file.absolutePath,
            PackageManager.GET_SIGNING_CERTIFICATES) ?: error("UPDATE_ARCHIVE")
        val current = installed(context)
        require(archive.packageName == context.packageName && archive.packageName == r.packageName)
        require(archive.longVersionCode == r.versionCode && archive.longVersionCode > current.longVersionCode)
        require(archive.applicationInfo?.minSdkVersion == r.minSdk && Build.VERSION.SDK_INT >= r.minSdk)
        // First release keeps the same key. Rotation/lineage is deliberately not accepted implicitly.
        val signers = signers(archive)
        require(signers.isNotEmpty() && signers == signers(current)) { "UPDATE_SIGNER" }
        ZipFile(file).use { zip ->
            val entries = zip.entries()
            var nativeAbiPresent = false
            while (entries.hasMoreElements()) {
                checkpoint()
                val entry = entries.nextElement()
                if (!entry.isDirectory && entry.name.startsWith("lib/${r.abi}/") && entry.name.endsWith(".so"))
                    nativeAbiPresent = true
            }
            require(nativeAbiPresent) { "UPDATE_ABI" }
        }
        checkpoint()
    }

    private fun installed(context: Context): PackageInfo = context.packageManager.getPackageInfo(
        context.packageName, PackageManager.GET_SIGNING_CERTIFICATES)

    private fun signers(info: PackageInfo): Set<String> = info.signingInfo?.apkContentsSigners
        ?.map { signature -> MessageDigest.getInstance("SHA-256").digest(signature.toByteArray())
            .joinToString("") { "%02x".format(it) } }?.toSet().orEmpty()

    private fun connect(url: String, io: UpdateOperation, accept: String): HttpURLConnection {
        require(deployment.trustedUrl(url)) { "UPDATE_ORIGIN" }
        io.check()
        val connection = URL(url).openConnection() as HttpURLConnection
        io.connection(connection)
        connection.instanceFollowRedirects = false
        connection.useCaches = false
        connection.requestMethod = "GET"
        connection.connectTimeout = IO_TIMEOUT_MS
        connection.readTimeout = IO_TIMEOUT_MS
        connection.setRequestProperty("Accept", accept)
        connection.setRequestProperty("Accept-Encoding", "identity")
        connection.setRequestProperty("Cache-Control", "no-cache")
        io.check()
        if (connection.responseCode != 200) throw IOException("UPDATE_HTTP_${connection.responseCode}")
        require(connection.contentEncoding.isNullOrBlank() || connection.contentEncoding.equals("identity", true))
        io.check()
        return connection
    }

}

package xyz.terlimo.test

import android.content.Context
import android.content.Intent
import android.os.Handler
import android.os.Looper
import kotlinx.coroutines.*
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import org.json.JSONObject
import java.io.File

/** Application-scoped only for update workflow. No identity/payment namespace or service control. */
internal class AppUpdateCoordinator private constructor(context: Context) {
    private val app = context.applicationContext
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private val prefs = app.getSharedPreferences("app_update_workflow_v1", Context.MODE_PRIVATE)
    private val deployment = runCatching {
        AppUpdateDeployment(BuildConfig.UPDATE_MANIFEST_URL, BuildConfig.UPDATE_CHANNEL, BuildConfig.UPDATE_PATH_PREFIX)
    }.getOrNull()
    private val core = deployment?.let(::AppUpdate)
    private val flow = AppUpdateFlow()
    private var release: AppUpdateRelease? = null
    private var file: File? = null
    private var task: Job? = null
    private var checkJob: Job? = null
    private var generation = 0L
    private var initialized = false
    private val writes = Mutex()
    private var offerSerial = 0L
    private var offerSequence = 0L
    private val listeners = linkedSetOf<(State) -> Unit>()
    data class State(val stage: String = "idle", val text: String = "", val release: AppUpdateRelease? = null,
        val progress: Int? = null, val offerSerial: Long = 0,
        val busy: Boolean = false)
    var state = State(); private set

    init {
        scope.launch {
            withContext(Dispatchers.IO) {
                flow.dismissedVersion = prefs.getLong("dismissed", 0)
                flow.notifiedVersion = prefs.getLong("notified", 0)
                flow.stage = prefs.getString("stage", "idle") ?: "idle"
                release = deployment?.let { d -> runCatching {
                    AppUpdateContract.parse(prefs.getString("release", "")!!.toByteArray(), d)
                }.getOrNull() }
                flow.targetVersion = release?.versionCode ?: 0
                file = prefs.getString("file", null)?.let(::File)?.takeIf {
                    runCatching { it.canonicalFile.parentFile == File(app.cacheDir, "updates").canonicalFile }.getOrDefault(false)
                }
                // No in-process download exists at initialization. Remove only updater partials.
                File(app.cacheDir, "updates").listFiles()?.filter { it.name.startsWith("update-") && it.name.endsWith(".partial") }
                    ?.forEach { it.delete() }
            }
            reconcile()
            initialized = true
            pump()
        }
    }
    fun add(listener: (State) -> Unit) { listeners.add(listener); listener(state) }
    fun remove(listener: (State) -> Unit) { listeners.remove(listener) }
    private fun publish(text: String = state.text, progress: Int? = null) {
        state = State(flow.stage, text, release, progress, offerSerial, task?.isActive == true)
        listeners.toList().forEach { it(state) }
    }
    fun connection(id: String) { flow.connection(id); pump() }
    fun checkManually() { flow.manual(); pump() }
    private fun pump() {
        if (!initialized || checkJob?.isActive == true) return
        val request = flow.next() ?: return
        checkJob = scope.launch {
            try {
                val updater = core
                if (updater == null) { if (request.manual) publish("Источник обновлений пока не настроен"); return@launch }
                val candidate = updater.fetch()
                if (updater.eligible(app, candidate)) {
                    // Keep explicit download/permission/install immutable; finish this check without replacing its target.
                    if (task?.isActive != true && flow.stage !in setOf("permission", "installing")) {
                        if (release != candidate) {
                            file?.delete(); file = null
                            release = candidate; flow.targetVersion = candidate.versionCode; flow.stage = "available"
                        }
                        if (flow.shouldOffer(candidate.versionCode, request.manual)) {
                            offerSerial = ++offerSequence
                        }
                        save(); publish("Доступно обновление ${candidate.versionName}")
                    }
                } else if (request.manual) publish("Установлена актуальная совместимая версия")
            } catch (cancelled: CancellationException) { throw cancelled }
            catch (_: Exception) { if (request.manual) publish("Не удалось проверить обновление. Повторите позже.") }
            finally { flow.finished(); checkJob = null; pump() }
        }
    }
    fun offered() {
        flow.notifiedVersion = release?.versionCode ?: 0
        prefs.edit().putLong("notified", flow.notifiedVersion).apply()
        offerSerial = 0
        state = state.copy(offerSerial = 0)
    }
    fun later() {
        flow.later(); offerSerial = 0
        prefs.edit().putLong("dismissed", flow.dismissedVersion).apply()
        publish("Обновление отложено. Оно доступно в Настройках.")
    }
    fun download() {
        val target = release ?: return
        val updater = core ?: return
        if (!initialized || task?.isActive == true) return
        val epoch = ++generation
        task = scope.launch {
            try {
                file?.delete(); file = null
                flow.stage = "downloading"; check(save()); publish("Скачивание обновления…", 0)
                val ready = updater.download(app, target) { received, total ->
                    scope.launch { if (generation == epoch && flow.stage == "downloading")
                        publish("Скачивание обновления…", (received * 100 / total).toInt()) }
                }
                ensureActive()
                file = ready; flow.stage = "ready"; check(save()); publish("APK проверен. Нажмите «Установить».")
            } catch (cancelled: CancellationException) { throw cancelled }
            catch (_: Exception) { if (generation == epoch) {
                flow.stage = "retry"; save(); publish("Не удалось скачать или проверить APK. Повторите загрузку.")
            } }
            finally { if (generation == epoch) { task = null; publish() } }
        }
    }
    fun cancelDownload() {
        if (flow.stage != "downloading") return
        generation++; task?.cancel(); task = null; flow.cancelled()
        scope.launch { save(); publish("Загрузка отменена. Установленная версия сохранена.") }
    }
    /** Only called from the explicit install workflow; UI owns VPN warning before this call. */
    fun installIntent(result: (Intent?) -> Unit, permission: () -> Unit) {
        val updater = core ?: return
        val target = release ?: return
        val ready = file ?: return
        if (task?.isActive == true) return
        task = scope.launch {
            try {
                updater.verifyDownloaded(app, ready, target)
                if (!updater.canRequestInstall(app)) {
                    flow.stage = "permission"; check(save())
                    publish("Разрешите установку из этого источника в настройках Android.")
                    permission()
                } else {
                    val intent = updater.installerIntent(app, ready, target, "${app.packageName}.updates")
                    flow.stage = "installing"; check(save()); result(intent)
                }
            } catch (_: Exception) { flow.stage = "retry"; save(); publish("Установка недоступна. Проверьте или скачайте APK заново."); result(null) }
            finally { task = null; publish() }
        }
    }
    fun launchFailed() { flow.stage = "ready"; scope.launch { save(); publish("Не удалось открыть установщик Android. Попробуйте ещё раз.") } }
    fun resumed(onPermissionReady: () -> Unit) {
        if (!initialized || task?.isActive == true) return
        task = scope.launch {
            try {
                val wasPermission = flow.stage == "permission"
                val allowed = wasPermission && core?.canRequestInstall(app) == true
                if (wasPermission) flow.permissionReturned(allowed)
                reconcile()
                if (wasPermission && allowed && flow.stage == "ready") onPermissionReady()
            } finally { task = null; publish() }
        }
    }
    private suspend fun reconcile() {
        val installed = app.packageManager.getPackageInfo(app.packageName, 0).longVersionCode
        val previous = flow.stage
        flow.reconcile(installed, file?.isFile == true)
        if (flow.stage == "installed") {
            file?.delete(); file = null; release = null; flow.targetVersion = 0; offerSerial = 0
            publish("Обновление установлено")
        } else if (flow.stage in setOf("ready", "permission", "installing")) {
            try {
                core!!.verifyDownloaded(app, file!!, release!!)
                if (flow.stage == "installing") flow.stage = "ready"
                publish(if (previous == "installing") "Установка не завершена. Можно повторить." else "APK проверен. Можно установить обновление.")
            } catch (_: Exception) { file?.delete(); file = null; flow.stage = "retry"; publish("Файл обновления недоступен. Скачайте заново.") }
        } else if (flow.stage == "retry") publish("Загрузка не завершена. Нажмите «Повторить загрузку».")
        else publish()
        save()
    }
    private suspend fun save(): Boolean = writes.withLock {
        val target = release
        val encoded = target?.let { r -> JSONObject().put("schema", 1).put("channel", r.channel)
            .put("packageName", r.packageName).put("versionCode", r.versionCode).put("versionName", r.versionName)
            .put("minSdk", r.minSdk).put("abi", r.abi).put("apkUrl", r.apkUrl).put("sizeBytes", r.sizeBytes)
            .put("sha256", r.sha256).apply { r.releaseNotes?.let { put("releaseNotes", it) } }.toString() }
        val stage = flow.stage; val path = file?.absolutePath
        withContext(Dispatchers.IO) {
            runCatching { prefs.edit().putString("release", encoded).putString("file", path).putString("stage", stage).commit() }.getOrDefault(false)
        }
    }
    companion object {
        @Volatile private var instance: AppUpdateCoordinator? = null
        fun get(context: Context): AppUpdateCoordinator = instance ?: synchronized(this) {
            instance ?: AppUpdateCoordinator(context).also { instance = it }
        }
        fun serviceConnected(context: Context, attempt: String) {
            Handler(Looper.getMainLooper()).post { get(context).connection(attempt) }
        }
    }
}

package xyz.terlimo.test

import android.app.Activity
import android.app.AlertDialog
import android.content.Intent
import android.net.Uri
import android.provider.Settings
import android.widget.Button
import android.widget.LinearLayout
import android.widget.TextView

/** Activity presentation only. Coordinator survives recreation; no activity retained by service. */
internal class AppUpdateUi(private val activity: Activity) {
    private val coordinator = AppUpdateCoordinator.get(activity)
    private var visible = false
    private var offered = 0L
    private var dialog: AlertDialog? = null
    private var panel: LinearLayout? = null
    private val listener: (AppUpdateCoordinator.State) -> Unit = { render(it) }
    fun panel(): LinearLayout = LinearLayout(activity).apply {
        orientation = LinearLayout.VERTICAL
        panel = this
        render(coordinator.state)
    }
    fun start() { visible = true; coordinator.add(listener) }
    fun stop() { visible = false; coordinator.remove(listener); dialog?.dismiss(); dialog = null }
    fun resume() { coordinator.resumed { if (visible) install() } }
    private fun render(state: AppUpdateCoordinator.State) {
        panel?.apply {
            removeAllViews()
            addView(TextView(activity).apply { text = "Обновление приложения"; textSize = 20f })
            addView(TextView(activity).apply {
                text = listOfNotNull(state.text.takeIf { it.isNotBlank() }, state.release?.let {
                    "Версия ${it.versionName} · ${it.sizeBytes / 1024} КБ"
                }, state.progress?.let { "$it%" }).joinToString("\n")
            })
            addView(Button(activity).apply {
                text = "Проверить обновления"; setOnClickListener { coordinator.checkManually() }
            })
            if (state.release != null) {
                addView(Button(activity).apply {
                    text = when (state.stage) {
                        "downloading" -> "Отменить загрузку"
                        "ready", "permission", "installing" -> "Установить"
                        "retry" -> "Повторить загрузку"
                        else -> "Обновить"
                    }
                    isEnabled = !state.busy || state.stage == "downloading"
                    setOnClickListener {
                        when (state.stage) {
                            "downloading" -> coordinator.cancelDownload()
                            "ready", "permission", "installing" -> install()
                            else -> coordinator.download()
                        }
                    }
                })
                addView(Button(activity).apply { text = "Позже"; setOnClickListener { coordinator.later() } })
            }
        }
        if (!visible || activity.isFinishing || state.release == null || state.offerSerial == 0L ||
            offered == state.offerSerial || dialog?.isShowing == true || state.busy ||
            state.stage !in setOf("available", "retry", "ready")) return
        offered = state.offerSerial
        coordinator.offered()
        val release = state.release
        dialog = AlertDialog.Builder(activity).setTitle("Доступно обновление")
            .setMessage("Версия ${release.versionName} · ${release.sizeBytes / 1024} КБ")
            .setPositiveButton(if (state.stage == "ready") "Установить" else "Обновить") { _, _ ->
                dialog = null
                if (state.stage == "ready") install() else coordinator.download()
            }.setNegativeButton("Позже") { _, _ -> dialog = null; coordinator.later() }
            .setOnCancelListener { dialog = null; coordinator.later() }.create().also { it.show() }
    }
    private fun install() {
        if (!visible || activity.isFinishing) return
        // Even with VPN off, disclose Android confirmation and explicit user-controlled install.
        dialog = AlertDialog.Builder(activity).setTitle("Установить обновление?")
            .setMessage("Откроется установщик Android. Приложение будет перезапущено, работающий VPN может отключиться. Данные сохранятся. Подключением после установки управляете вы.")
            .setPositiveButton("Установить сейчас") { _, _ ->
                dialog = null
                coordinator.installIntent(result = { intent ->
                    if (intent != null) {
                        if (!visible) coordinator.launchFailed()
                        else runCatching { activity.startActivity(intent) }.onFailure { coordinator.launchFailed() }
                    }
                }, permission = {
                    if (!visible) coordinator.launchFailed()
                    else runCatching {
                        activity.startActivity(Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES,
                            Uri.parse("package:${activity.packageName}")))
                    }.onFailure { coordinator.launchFailed() }
                })
            }.setNegativeButton("Позже") { _, _ -> dialog = null; coordinator.later() }
            .create().also { it.show() }
    }
}

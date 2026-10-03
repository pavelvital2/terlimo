package xyz.terlimo.test

import android.app.AlertDialog
import android.content.Context
import android.os.Build
import android.text.InputType
import android.view.View
import android.view.inputmethod.EditorInfo
import android.widget.EditText
import android.widget.Button
import android.widget.LinearLayout

/** Editor policy only. Signature, seed schema, revision and environment validation belong to native. */
internal object RecoveryCodeUi {
    private val SEGMENT = Regex("^[A-Za-z0-9_-]+$")

    fun normalizeCode(raw: String): String? {
        val code = raw.trim()
        if (code.isEmpty() || code.length > 3500 || code.any { it.code !in 33..126 }) return null
        val parts = code.split('.')
        if (parts.size != 3 || parts[0] != "TR1" ||
            !parts[1].matches(SEGMENT) || !parts[2].matches(SEGMENT)) return null
        return code
    }

    fun canApply(usable: Boolean, workingVpn: Boolean, busy: Boolean = false,
        serviceActive: Boolean = false): Boolean =
        unavailableReason(usable, workingVpn, busy, serviceActive) == null

    fun unavailableReason(usable: Boolean, workingVpn: Boolean, busy: Boolean = false,
        serviceActive: Boolean = false): String? = when {
        !usable -> "RECOVERY_UNAVAILABLE"
        busy -> "RECOVERY_BUSY"
        workingVpn || serviceActive -> "RECOVERY_DISCONNECT_REQUIRED"
        else -> null
    }

    fun textForStatus(code: String): String = when (code) {
        "RECOVERY_UNAVAILABLE" -> "Восстановление подключения недоступно в этой версии приложения."
        "RECOVERY_INVALID" -> "Код восстановления неполный или неверный. Вставьте целый код из одного сообщения бота."
        "RECOVERY_SIGNATURE" -> "Подпись кода восстановления не подтверждена. Получите новый код у бота."
        "RECOVERY_ENVIRONMENT" -> "Код восстановления предназначен для другой среды приложения."
        "RECOVERY_STALE" -> "Код восстановления устарел. Получите актуальный код у бота."
        "RECOVERY_NETWORK" -> "Не удалось проверить новое подключение. Сохранённые настройки не изменены."
        "RECOVERY_PERSIST" -> "Не удалось сохранить подключение. Сохранённые настройки не изменены."
        "RECOVERY_CANCELLED" -> "Восстановление отменено. Сохранённые настройки не изменены."
        "RECOVERY_SUCCESS" -> "Подключение восстановлено и сохранено. VPN можно подключить обычной кнопкой."
        "RECOVERY_DISCONNECT_REQUIRED" -> "Сначала нажмите «Отключить» и дождитесь остановки подключения."
        "RECOVERY_BUSY" -> "Восстановление подключения уже выполняется."
        "RECOVERY_RUNNING" -> "Проверяем новое подключение…"
        else -> "Не удалось восстановить подключение. Сохранённые настройки не изменены."
    }

    fun showEditor(context: Context, submit: (String) -> Unit,
        cancel: () -> Unit = {}, openTelegram: (() -> Unit)? = null,
        openWebsite: (() -> Unit)? = null): AlertDialog {
        val input = EditText(context).apply {
            hint = "Вставьте код из сообщения бота"
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD or
                InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
            imeOptions = EditorInfo.IME_FLAG_NO_EXTRACT_UI or EditorInfo.IME_FLAG_NO_PERSONALIZED_LEARNING
            isSaveEnabled = false
            isSaveFromParentEnabled = false
            importantForAutofill = View.IMPORTANT_FOR_AUTOFILL_NO_EXCLUDE_DESCENDANTS
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                importantForContentCapture = View.IMPORTANT_FOR_CONTENT_CAPTURE_NO_EXCLUDE_DESCENDANTS
            }
        }
        val editor = LinearLayout(context).apply {
            orientation = LinearLayout.VERTICAL
            addView(input)
            openTelegram?.let { open -> addView(Button(context).apply {
                text = "Получить в Telegram"
                setOnClickListener { open() }
            }) }
            openWebsite?.let { open -> addView(Button(context).apply {
                text = "Получить на сайте"
                setOnClickListener { open() }
            }) }
        }
        val dialog = AlertDialog.Builder(context).setTitle("Восстановить подключение")
            .setMessage("Вставьте целый код восстановления и нажмите «Применить».")
            .setView(editor)
            .setPositiveButton("Применить", null)
            .setNegativeButton("Отмена") { _, _ -> cancel() }
            .create()
        dialog.setOnCancelListener { cancel() }
        dialog.setOnDismissListener { input.text?.clear() }
        dialog.setOnShowListener {
            dialog.getButton(AlertDialog.BUTTON_POSITIVE).setOnClickListener {
                val code = normalizeCode(input.text.toString())
                if (code == null) {
                    input.error = textForStatus("RECOVERY_INVALID")
                } else {
                    input.text?.clear()
                    dialog.dismiss()
                    submit(code)
                }
            }
        }
        dialog.show()
        return dialog
    }
}

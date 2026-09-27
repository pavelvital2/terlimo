package xyz.terlimo.test

import android.app.Activity
import android.os.Bundle
import android.text.InputType
import android.widget.*

class RetentionSettingsActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val prefs = getSharedPreferences("transport_retention", MODE_PRIVATE)
        val root = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; setPadding(32, 32, 32, 48) }
        root.addView(TextView(this).apply { text = "Работа с выключенным экраном"; textSize = 24f })
        val modes = RadioGroup(this)
        val labels = listOf("Сбалансированный", "Усиленное удержание", "Экономия батареи")
        RetentionMode.entries.forEachIndexed { index, mode ->
            modes.addView(RadioButton(this).apply { id = index + 1; text = labels[index]; minHeight = dp(48) })
            if (mode == RetentionMode.fromStored(prefs.getString("mode", null))) modes.check(index + 1)
        }
        root.addView(modes)
        root.addView(TextView(this).apply {
            text = "Усиленное удержание не даёт процессору засыпать и удерживает Wi-Fi при выключенном экране. Это увеличивает расход батареи; ограничения Android всё равно могут влиять на связь."
        })
        root.addView(TextView(this).apply { text = "Экономия: пауза после выключения экрана, минут" })
        val pause = EditText(this).apply {
            inputType = InputType.TYPE_CLASS_NUMBER
            setText(prefs.getInt("pause_minutes", SleepRetentionPolicy.DEFAULT_PAUSE_MINUTES).toString())
        }
        root.addView(pause)
        val timed = CheckBox(this).apply { text = "Вместо задержки — пауза сразу и включение по таймеру"; isChecked = prefs.getBoolean("timed_resume", false) }
        root.addView(timed)
        root.addView(TextView(this).apply { text = "Включить через, минут (0 — оставить связь включённой)" })
        val resume = EditText(this).apply { inputType = InputType.TYPE_CLASS_NUMBER; setText(prefs.getInt("resume_minutes", 5).toString()) }
        root.addView(resume)
        root.addView(TextView(this).apply {
            text = "Во время паузы трафик остаётся заблокирован VPN. Включение экрана возобновляет связь. Настройки применяются при следующем подключении."
        })
        root.addView(Button(this).apply {
            text = "Сохранить"; minHeight = dp(48)
            setOnClickListener {
                val pauseMinutes = pause.text.toString().toIntOrNull()
                val resumeMinutes = resume.text.toString().toIntOrNull()
                if (pauseMinutes == null || pauseMinutes !in 0..SleepRetentionPolicy.MAX_MINUTES ||
                    resumeMinutes == null || resumeMinutes !in 0..SleepRetentionPolicy.MAX_MINUTES) {
                    Toast.makeText(this@RetentionSettingsActivity, "Укажите от 0 до 1440 минут", Toast.LENGTH_SHORT).show()
                } else {
                    val mode = RetentionMode.entries.getOrNull(modes.checkedRadioButtonId - 1) ?: RetentionMode.BALANCED
                    prefs.edit().putString("mode", mode.storedValue).putInt("pause_minutes", pauseMinutes)
                        .putBoolean("timed_resume", timed.isChecked).putInt("resume_minutes", resumeMinutes).apply()
                    finish()
                }
            }
        })
        setContentView(ScrollView(this).apply { addView(root) })
    }
    private fun dp(value: Int) = (value * resources.displayMetrics.density).toInt()
}
